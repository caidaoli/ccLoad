package app

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func nestedJSONObject(depth int, inner string) []byte {
	if depth <= 0 {
		return []byte(inner)
	}
	return []byte(`{"k":` + string(nestedJSONObject(depth-1, inner)) + `}`)
}

func targetPath(depth int) []string {
	path := make([]string, 0, depth+1)
	for range depth {
		path = append(path, "k")
	}
	path = append(path, "target")
	return path
}

func TestParseOrderedJSON_DepthBoundary(t *testing.T) {
	t.Parallel()

	leaf := `{"target":1}`
	atLimit := nestedJSONObject(jsonWalkMaxDepth, leaf)
	root, err := parseOrderedJSON(atLimit)
	if err != nil {
		t.Fatalf("depth %d: parse failed: %v", jsonWalkMaxDepth, err)
	}
	replacement, err := parseOrderedJSON([]byte(`2`))
	if err != nil {
		t.Fatalf("replacement parse: %v", err)
	}
	ok, conflict := root.overridePath(targetPath(jsonWalkMaxDepth), replacement)
	if !ok || conflict {
		t.Fatalf("depth %d: overridePath failed ok=%v conflict=%v", jsonWalkMaxDepth, ok, conflict)
	}
	if gjson.GetBytes(root.render(), strings.Repeat("k.", jsonWalkMaxDepth)+"target").Int() != 2 {
		t.Fatalf("depth %d: expected rewritten leaf, got %s", jsonWalkMaxDepth, root.render())
	}

	overLimit := nestedJSONObject(jsonWalkMaxDepth+1, leaf)
	root, err = parseOrderedJSON(overLimit)
	if err != nil {
		t.Fatalf("depth %d: parse failed: %v", jsonWalkMaxDepth+1, err)
	}
	ok, conflict = root.overridePath(targetPath(jsonWalkMaxDepth+1), replacement)
	if ok {
		t.Fatalf("depth %d: overridePath should miss literal subtree ok=%v conflict=%v", jsonWalkMaxDepth+1, ok, conflict)
	}
	if !bytes.Equal(root.render(), overLimit) {
		t.Fatalf("depth %d: over-limit body changed: %s", jsonWalkMaxDepth+1, root.render())
	}
}

func TestRewriteJSONMembers_DepthBoundary(t *testing.T) {
	t.Parallel()

	rewriteTarget := func(body []byte) ([]byte, bool) {
		return rewriteJSONMembers(body, func(key string, _ gjson.Result) (string, bool) {
			if key == "target" {
				return "9", true
			}
			return "", false
		})
	}

	leaf := `{"target":1}`
	atLimit := nestedJSONObject(jsonWalkMaxDepth, leaf)
	got, changed := rewriteTarget(atLimit)
	if !changed {
		t.Fatalf("depth %d: expected rewrite to succeed", jsonWalkMaxDepth)
	}
	if gjson.GetBytes(got, strings.Repeat("k.", jsonWalkMaxDepth)+"target").Int() != 9 {
		t.Fatalf("depth %d: rewrite missed target: %s", jsonWalkMaxDepth, got)
	}

	overLimit := nestedJSONObject(jsonWalkMaxDepth+1, leaf)
	got, changed = rewriteTarget(overLimit)
	if changed {
		t.Fatalf("depth %d: rewrite should not change over-limit body, got %s", jsonWalkMaxDepth+1, got)
	}
	if !bytes.Equal(got, overLimit) {
		t.Fatalf("depth %d: body changed: %s", jsonWalkMaxDepth+1, got)
	}
}

func TestParseOrderedJSON_UnicodeEscapeKeyPreservedOnSiblingDirty(t *testing.T) {
	t.Parallel()

	body := []byte(`{"\u0061":1,"b":2}`)
	root, err := parseOrderedJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	replacement, err := parseOrderedJSON([]byte(`3`))
	if err != nil {
		t.Fatalf("replacement parse: %v", err)
	}
	if ok, conflict := root.overridePath([]string{"b"}, replacement); !ok || conflict {
		t.Fatalf("override b failed ok=%v conflict=%v", ok, conflict)
	}
	got := string(root.render())
	if !strings.Contains(got, `"\u0061"`) {
		t.Fatalf("unicode-escape key literal lost after sibling dirty: %s", got)
	}
	if strings.Contains(got, `"a":`) {
		t.Fatalf("key canonicalized to a: %s", got)
	}
	if gjson.GetBytes([]byte(got), "b").Int() != 3 {
		t.Fatalf("sibling override failed: %s", got)
	}
}

func TestApplyBodyRules_WideObjectCompletesQuickly(t *testing.T) {
	t.Parallel()

	const fieldCount = 4000
	var b strings.Builder
	b.WriteString(`{"z_last":0`)
	for i := range fieldCount {
		b.WriteString(`,"f`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`":`)
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteByte('}')
	body := []byte(b.String())

	rules := []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "z_last", Value: json.RawMessage(`1`)},
	}

	done := make(chan []byte, 1)
	go func() {
		done <- applyBodyRules("application/json", body, rules)
	}()

	select {
	case out := <-done:
		if gjson.GetBytes(out, "z_last").Int() != 1 {
			t.Fatalf("wide object override failed: z_last=%d", gjson.GetBytes(out, "z_last").Int())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wide object body rule parse exceeded 2s timeout")
	}
}

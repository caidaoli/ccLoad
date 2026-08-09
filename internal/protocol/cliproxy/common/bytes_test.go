package common

import "testing"

func TestJoinRawArray(t *testing.T) {
	tests := []struct {
		name  string
		items [][]byte
		want  string
	}{
		{name: "empty", want: "[]"},
		{name: "single", items: [][]byte{[]byte(`{"id":1}`)}, want: `[{"id":1}]`},
		{name: "multiple", items: [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`)}, want: `[{"id":1},{"id":2}]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := string(JoinRawArray(test.items)); got != test.want {
				t.Fatalf("JoinRawArray() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestSetRawArrayItems(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		path  string
		items [][]byte
		want  string
	}{
		{name: "empty", data: `{"items":[]}`, path: "items", want: `{"items":[]}`},
		{name: "single nested", data: `{"before":1,"request":{"contents":[]},"after":2}`, path: "request.contents", items: [][]byte{[]byte(`{"id":1}`)}, want: `{"before":1,"request":{"contents":[{"id":1}]},"after":2}`},
		{name: "single fallback", data: `{"items":[{"old":1},{"old":2}]}`, path: "items", items: [][]byte{[]byte(`{"id":1}`)}, want: `{"items":[{"id":1}]}`},
		{name: "multiple", data: `{"items":[]}`, path: "items", items: [][]byte{[]byte(`{"id":1}`), []byte(`{"id":2}`)}, want: `{"items":[{"id":1},{"id":2}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := SetRawArrayItems([]byte(test.data), test.path, test.items)
			if string(got) != test.want {
				t.Fatalf("SetRawArrayItems() = %s, want %s", got, test.want)
			}
		})
	}
}

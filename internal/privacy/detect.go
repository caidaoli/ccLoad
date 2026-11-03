// Detection patterns and validators are adapted from yetone/magpie at
// c74b932c8288e8aa103d0a231b57f3d71bd75b6d (MIT). See LICENSE.magpie.
package privacy

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/publicsuffix"
)

type detector struct {
	kind, category string
	re             *regexp.Regexp
	bound          string
	check          func(string) bool
	at             func(string, int, int) bool
	groups         []string
	gate           gate // set by NewPolicy
}

const (
	alnum       = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	tokenChars  = alnum + "_-"
	secretNames = `password|passwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|token|credential`
	secretChars = `[A-Za-z0-9_\-./+=~!@#%^&*]`
	labelSep    = `["']?[ \t]*(?:[:#=：][ \t]*)?`
	months      = `(?i:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?`
)

func pattern(kind, category, re, bound string, check func(string) bool) detector {
	return detector{kind: kind, category: category, re: regexp.MustCompile(re), bound: bound, check: check}
}

var builtinRules = []detector{
	pattern("PRIVATE_KEY", "", `-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----[\s\S]+?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`, "", nil),
	pattern("API_KEY", "", `sk-(?:ant-|proj-|or-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`, tokenChars, notSlug),
	pattern("API_KEY", "", `[a-z]{2,4}_sk_[A-Za-z0-9_-]{20,}|ark-[A-Za-z0-9][A-Za-z0-9_-]{15,}`, tokenChars, secretValue),
	pattern("API_KEY", "", `gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{35}|xox[abposr]-[0-9A-Za-z-]{10,}|(?:sk|rk)_live_[0-9A-Za-z]{20,}|(?:AKIA|ASIA)[0-9A-Z]{16}`, tokenChars, nil),
	pattern("API_KEY", "", `hf_[A-Za-z0-9]{30,}|gsk_[A-Za-z0-9]{40,}|xai-[A-Za-z0-9]{40,}|npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|dop_v1_[a-f0-9]{64}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}`, tokenChars, nil),
	pattern("TOKEN", "", `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, tokenChars, nil),
	pattern("PASSWORD", "", `[A-Za-z][A-Za-z0-9+.-]*://[^\s:@/?#"'<>]*:([^\s@/?#"'<>]{3,})@`, "", realValue),
	{kind: "SECRET", re: regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:` + secretNames + `)[A-Za-z0-9_.-]*["']?[ \t]*[:=][ \t]*["']?(` + secretChars + `{8,})`), check: secretValue, at: secretAt},
	{kind: "SECRET", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])[A-Za-z0-9_.-]*_(?:KEY|PASS)["']?[ \t]*[:=][ \t]*["']?(` + secretChars + `{8,})`), check: secretValue, at: secretAt},
	pattern("SECRET", "", `(?im)^[ \t]*(?:authorization|cookie)[ \t]*:[ \t]*(\S[^\r\n]{7,})`, "", realValue),
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])["']?key["']?[ \t]*[:=][ \t]*["']?(` + secretChars + `{16,})`), check: secretValue, at: secretAt},
	pattern("PASSWORD", "", `(?i)(?:密码|密碼|口令|PIN[ ]?[码碼]|安全[码碼]|パスワード|暗証番号|비밀번호|암호|비번)[ \t]*(?:[:：=][ \t]*|[为為是][ \t]*[:：]?[ \t]*|は[ \t]*|는[ \t]*)?["'“「]?(`+secretChars+`{3,63}[A-Za-z0-9_\-/+=~!@#%^&*])`, "", realValue),
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:密[钥鑰]|秘[钥鑰]|私[钥鑰]|金[钥鑰]|令牌|[权權]杖|授[权權][码碼]|秘密鍵|トークン|シークレット|APIキー|アクセスキー|토큰|비밀키|API[ ]?키|` + secretNames + `|api key|key)[ \t]*(?:[:：=][ \t]*|[为為是][ \t]*[:：]?[ \t]*|は[ \t]*|는[ \t]*)["'“「]?(` + secretChars + `{3,63}[A-Za-z0-9_\-/+=~!@#%^&*])`), check: secretValue, at: secretAt},
	{kind: "EMAIL", category: "personal", re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), bound: alnum + "_%+-", check: realEmail},
	pattern("PHONE", "personal", `(?:\+?86[- ]?)?1[3-9]\d{9}`, alnum, nil),
	pattern("ID_CARD", "personal", `[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]`, alnum, chineseID),
	pattern("BANK_CARD", "personal", `[3-6]\d{3}(?:[ -]?\d{4}){2,3}(?:[ -]?\d{1,3})?`, alnum, luhn),
	pattern("SSN", "ssn", `\d{3}-\d{2}-\d{4}`, alnum+"-", ssnOK),
	pattern("SSN", "ssn", `(?i:\bssn|\bsocial[ \t]+security(?:[ \t]+(?:number|no\.?|#))?)`+labelSep+`(?:(?i:is)[ \t]+)?["']?(\d{9}|\d{3} \d{2} \d{4})`, alnum, ssnOK),
	pattern("PASSPORT", "passport", `(?:(?i:\bpassport(?:[ \t]*(?:no\.?|number|num|#))?|\breisepass(?:nummer)?)|护照(?:号码|号)?|護照(?:號碼|号码|番号)?|パスポート(?:番号)?)`+labelSep+`(?:(?i:is|was)[ \t]+|是[ \t]*|は[ \t]*)?["']?([A-Z0-9]{6,9})`, alnum, passportOK),
	pattern("IBAN", "iban", `[A-Z]{2}\d{2}(?:[A-Z0-9]{11,30}|(?: [A-Z0-9]{4}){2,7}(?: [A-Z0-9]{1,3})?)`, alnum, ibanOK),
	pattern("BIRTHDAY", "birthday", `(?:(?i:\bdate[ \t]+of[ \t]+birth|\bbirth[ \t_-]?date|\bbirthday|\bdob\b|\bd\.o\.b\.?|\bborn(?:[ \t]+on)?|\bgeburtsdatum|\bgeboren(?:[ \t]+am)?)|生日|出生日期|出生年月日|出生于|出生於|出生|誕生日|生年月日)`+labelSep+`(?:(?i:is|was)[ \t]+|是[ \t]*|[为為][ \t]*|は[ \t]*)?["']?(\d{4}[-/.]\d{1,2}[-/.]\d{1,2}|\d{4}[ \t]*年[ \t]*\d{1,2}[ \t]*月[ \t]*\d{1,2}[ \t]*日?|\d{1,2}[-/.]\d{1,2}[-/.]\d{4}|`+months+`[ \t]+\d{1,2}(?:st|nd|rd|th)?,?[ \t]+\d{4}|\d{1,2}(?:st|nd|rd|th)?[ \t]+`+months+`,?[ \t]+\d{4})`, "0123456789", birthdayOK),
	pattern("MAC", "mac", `[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}|[0-9A-Fa-f]{2}(?:-[0-9A-Fa-f]{2}){5}`, alnum+":-", macOK),
	pattern("SERIAL", "serial", `(?:(?i:\bserial[ \t_-]?(?:number|num|no\.?|#)?|\bs/n|\bsn)|序列号|序列號|シリアル番号|(?i:seriennummer))["']?[ \t]*[:#=：][ \t]*["']?([A-Za-z0-9][A-Za-z0-9-]{4,39})`, alnum+"-", serialOK),
	pattern("USERNAME", "home", `(?:^|[^A-Za-z0-9_.~/\\-])(?:file://)?(?:/mnt/[a-z])?/(?:Users|home)/([^\s/\\:*?"'<>|,;()\[\]{}`+"`"+`$%]{1,64})`, "", userOK),
	pattern("USERNAME", "home", `(?:^|[^A-Za-z0-9])[A-Za-z]:(?:\\{1,4}|/)(?i:users|documents and settings)(?:\\{1,4}|/)([^\s/\\:*?"'<>|,;()\[\]{}$%]{1,64})`, "", userOK),
	{category: "userhost", re: regexp.MustCompile(`(?m)(?:^|[^A-Za-z0-9._%+-])([a-z_][a-z0-9_.-]{0,31})@([A-Za-z0-9][A-Za-z0-9.-]{0,252})(?::[~/]|:[ \t]*$|[ \t]+[~/]|[ \t]*[$%#>](?:[ \t]|$))`), groups: []string{"USERNAME", "HOSTNAME"}},
	{category: "userhost", re: regexp.MustCompile(`(?i:\b(?:ssh|scp|sftp|rsync|mosh|ssh-copy-id))\b[^\n@]{0,80}?[ \t"']([a-z_][a-z0-9_.-]{0,31})@([A-Za-z0-9][A-Za-z0-9.-]{0,252})`), groups: []string{"USERNAME", "HOSTNAME"}},
	pattern("HOSTNAME", "userhost", `(?im)^[ \t]*(?:hostname|computer_name|machine_name|主机名|主機名)[ \t]*[:=：][ \t]*["']?([A-Za-z0-9][A-Za-z0-9.-]{1,252})`, "", hostOK),
	{kind: "IP", category: "ip", re: regexp.MustCompile(`(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?:\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}`), bound: alnum + "._", check: publicIP, at: ipAt},
	pattern("IP", "ip", `[0-9A-Fa-f]{1,4}(?::[0-9A-Fa-f]{0,4}){2,7}`, alnum+":_", publicIP),
	pattern("BUCKET", "bucket", `(?i:\b(?:s3a?|s3n|gs|oss|cosn?|obs|r2|ks3|bos|tos|az|azure|wasbs?|abfss?)://)([A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9])`, "", bucketOK),
	pattern("BUCKET", "bucket", `(?:^|[^A-Za-z0-9.-])([a-z0-9][a-z0-9.-]{1,61}[a-z0-9])\.(?:s3(?:[.-](?:dualstack\.)?[a-z0-9-]+)?\.amazonaws\.com|storage\.googleapis\.com|oss-[a-z0-9-]+\.aliyuncs\.com|cos\.[a-z0-9-]+\.myqcloud\.com|(?:blob|dfs)\.core\.windows\.net)`, "", bucketOK),
	pattern("BUCKET", "bucket", `(?:s3(?:[.-][a-z0-9-]+)?\.amazonaws\.com|storage\.googleapis\.com|[a-z0-9]{3,24}\.blob\.core\.windows\.net)/([A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9])`, "", bucketOK),
}

// matches returns the rule's submatch indices in text, searching only the
// windows its gate allows.
func (r detector) matches(text, lower string, unusualFold bool) [][]int {
	var out [][]int
	for _, w := range r.gate.windows(text, lower, unusualFold) {
		for _, indices := range r.re.FindAllStringSubmatchIndex(text[w[0]:w[1]], maxMatches+1-len(out)) {
			for i := range indices {
				if indices[i] >= 0 {
					indices[i] += w[0]
				}
			}
			out = append(out, indices)
		}
		if len(out) > maxMatches {
			break
		}
	}
	return out
}

func (r detector) find(text, lower string, unusualFold bool) []match {
	var out []match
	for _, indices := range r.matches(text, lower, unusualFold) {
		if len(r.groups) != 0 {
			for i, kind := range r.groups {
				a, b := indices[2+i*2], indices[3+i*2]
				if a < 0 || a == b {
					continue
				}
				ok := kind == "USERNAME" && userOK(text[a:b]) || kind == "HOSTNAME" && hostOK(text[a:b])
				if ok {
					out = append(out, match{a, b, kind, r.category})
				}
			}
			continue
		}
		a, b := indices[0], indices[1]
		if len(indices) > 2 {
			a, b = -1, -1
			for i := 2; i < len(indices); i += 2 {
				if indices[i] >= 0 {
					a, b = indices[i], indices[i+1]
					break
				}
			}
		}
		if a >= 0 && r.kind == "IBAN" {
			for !ibanOK(text[a:b]) {
				i := strings.LastIndexByte(text[a:b], ' ')
				if i < 0 {
					break
				}
				b = a + i
			}
		}
		if a < 0 || a == b || r.bound != "" && (a > 0 && strings.IndexByte(r.bound, text[a-1]) >= 0 || b < len(text) && strings.IndexByte(r.bound, text[b]) >= 0) ||
			r.check != nil && !r.check(text[a:b]) || r.at != nil && !r.at(text, a, b) {
			continue
		}
		if r.kind == "PHONE" && !phoneAt(text, a, b) {
			continue
		}
		out = append(out, match{a, b, r.kind, r.category})
	}
	return out
}

func realValue(v string) bool {
	return v != "" && !strings.ContainsAny(v[:1], "$%{<[*") && strings.Trim(v, "*xX.") != ""
}

func secretValue(v string) bool {
	if !realValue(v) || pathValue(v) || versionPattern.MatchString(v) || namesCode(v) {
		return false
	}
	return strings.IndexFunc(v, unicode.IsLetter) >= 0 && (strings.ContainsAny(v, "0123456789") || len(v) >= 16) || len(v) >= 6 && strings.Trim(v, "0123456789") == ""
}

var versionPattern = regexp.MustCompile(`^[~^<>=v]*\d+(?:\.\d+)+(?:[-+._]?[A-Za-z0-9]+)*[.,]?$`)
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func namesCode(v string) bool {
	if !identifierPattern.MatchString(v) || !strings.ContainsAny(v, "_.") && !unicode.IsLower(rune(v[0])) {
		return false
	}
	var parts strings.Builder
	for i, r := range v {
		if r == '_' || r == '.' {
			parts.WriteByte(' ')
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' && v[i-1] >= 'a' && v[i-1] <= 'z' {
			parts.WriteByte(' ')
		}
		parts.WriteRune(unicode.ToLower(r))
	}
	words := strings.Fields(parts.String())
	if len(words) < 2 {
		return false
	}
	// Word-shaped names such as i18n keys and snake_case fields are code, not credentials.
	if wordShaped(v) {
		return true
	}
	for _, w := range words {
		if strings.Contains(" token tokens secret secrets password passwd pwd key keys apikey credential credentials ", " "+strings.TrimRight(w, "0123456789")+" ") {
			return true
		}
	}
	return false
}

var wordSegment = regexp.MustCompile(`^[a-z]+(?:[A-Z][a-z]+)*$`)

// wordShaped reports whether every _/. segment is a lowercase or lowerCamel
// word run; random tokens almost always contain uppercase runs or digits.
func wordShaped(v string) bool {
	for _, seg := range strings.FieldsFunc(v, func(r rune) bool { return r == '_' || r == '.' }) {
		if !wordSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

func pathValue(v string) bool {
	return strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || strings.HasPrefix(v, "/") && strings.Contains(v[1:], "/")
}

func secretAt(text string, a, b int) bool {
	if strings.Trim(text[a:b], "0123456789") == "" {
		after := strings.TrimLeft(text[b:], " \t")
		for _, unit := range []string{"位", "毫秒", "秒", "分钟", "字节", "bits", "bytes", "tokens", "ms"} {
			if strings.HasPrefix(after, unit) {
				return false
			}
		}
	}
	if b < len(text) && (text[b] == '(' || text[b] == '[') || strings.HasPrefix(text[a:b], "=") {
		return false
	}
	before := strings.TrimRight(text[:a], " \t\"'\\:=")
	start := strings.LastIndexAny(before, " \t\n\"'") + 1
	name := strings.ToLower(before[start:])
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "public_key") || strings.Contains(name, "_path") || strings.Contains(name, "_file") || strings.Contains(name, "max_token") || strings.Contains(name, "tokenizer") {
		return false
	}
	for _, ext := range []string{".go", ".js", ".ts", ".py", ".md", ".rs", ".java"} {
		if strings.HasSuffix(name, ext) {
			return false
		}
	}
	return true
}

func notSlug(v string) bool {
	if !realValue(v) {
		return false
	}
	// Names such as sk-this-is-a-public-documentation-example are slugs.
	parts := strings.Split(v, "-")
	if len(parts) >= 4 && v == strings.ToLower(v) {
		long := false
		for _, p := range parts {
			long = long || len(p) > 15
		}
		return long
	}
	return true
}

func realEmail(v string) bool {
	user, host, ok := strings.Cut(v, "@")
	if !ok {
		return false
	}
	if user == "git" && (host == "github.com" || host == "gitlab.com" || host == "bitbucket.org") {
		return false
	}
	_, icann := publicsuffix.PublicSuffix(strings.ToLower(host))
	return icann && !strings.Contains(host, "..")
}

func phoneAt(text string, a, b int) bool {
	if b-a > 11 {
		return true
	}
	before := strings.ToLower(strings.TrimRight(text[max(0, a-64):a], " \t\"'“「:=：是为為"))
	for _, label := range []string{"order", "order_id", "id", "uid", "no.", "#", "ticket", "invoice", "订单号", "编号", "流水号", "工单号"} {
		if strings.HasSuffix(before, label) {
			return false
		}
	}
	return true
}

func chineseID(v string) bool {
	weights := []int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i, w := range weights {
		sum += int(v[i]-'0') * w
	}
	return strings.ToUpper(v[17:]) == string("10X98765432"[sum%11])
}

func luhn(v string) bool {
	d := strings.NewReplacer(" ", "", "-", "").Replace(v)
	if len(d) < 13 || len(d) > 19 || strings.Trim(d, d[:1]) == "" {
		return false
	}
	sum := 0
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if (len(d)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

func ssnOK(v string) bool {
	d := strings.NewReplacer("-", "", " ", "").Replace(v)
	return len(d) == 9 && d[:3] != "000" && d[:3] != "666" && d[0] != '9' && d[3:5] != "00" && d[5:] != "0000" && d != "123456789" && d != "078051120" && d != "219099999" && strings.Trim(d, d[:1]) != ""
}

func passportOK(v string) bool {
	n := 0
	for _, r := range v {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n >= 5 && strings.Trim(v, v[:1]) != ""
}

var ibanLengths = map[string]int{"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BI": 27, "BR": 29, "BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DJ": 27, "DK": 18, "DO": 28, "EE": 20, "EG": 29, "ES": 24, "FI": 18, "FK": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28, "HN": 28, "HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20, "LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "LY": 25, "MC": 27, "MD": 24, "ME": 22, "MK": 19, "MN": 20, "MR": 27, "MT": 31, "MU": 30, "NI": 28, "NL": 18, "NO": 15, "OM": 23, "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29, "RO": 24, "RS": 22, "RU": 33, "SA": 24, "SC": 31, "SD": 18, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "SO": 23, "ST": 25, "SV": 28, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20, "YE": 30}

func ibanOK(v string) bool {
	v = strings.ReplaceAll(v, " ", "")
	if len(v) < 4 || ibanLengths[v[:2]] != len(v) {
		return false
	}
	rest := v[4:] + v[:4]
	mod := 0
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			mod = (mod*10 + int(r-'0')) % 97
		} else if r >= 'A' && r <= 'Z' {
			mod = (mod*100 + int(r-'A') + 10) % 97
		} else {
			return false
		}
	}
	return mod == 1
}

var dateNumbers = regexp.MustCompile(`\d+`)

func birthdayOK(v string) bool {
	numbers := dateNumbers.FindAllString(v, -1)
	integer := func(v string) int { n, _ := strconv.Atoi(v); return n }
	valid := func(y, m, d int) bool {
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		return y >= 1900 && t.Year() == y && int(t.Month()) == m && t.Day() == d && t.Before(time.Now().AddDate(-1, 0, 0))
	}
	if len(numbers) == 2 {
		for i, m := range []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"} {
			if strings.Contains(strings.ToLower(v), m) {
				return valid(integer(numbers[1]), i+1, integer(numbers[0]))
			}
		}
	}
	if len(numbers) != 3 {
		return false
	}
	if len(numbers[0]) == 4 {
		return valid(integer(numbers[0]), integer(numbers[1]), integer(numbers[2]))
	}
	return valid(integer(numbers[2]), integer(numbers[0]), integer(numbers[1])) || valid(integer(numbers[2]), integer(numbers[1]), integer(numbers[0]))
}

func macOK(v string) bool {
	v = strings.ToLower(strings.ReplaceAll(v, "-", ":"))
	for _, sample := range []string{"00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "00:11:22:33:44:55", "aa:bb:cc:dd:ee:ff", "01:23:45:67:89:ab", "12:34:56:78:9a:bc"} {
		if v == sample {
			return false
		}
	}
	return strings.ContainsAny(v, "abcdef")
}
func serialOK(v string) bool {
	return len(v) >= 6 && strings.ContainsAny(v, "0123456789") && strings.Trim(v, v[:1]) != ""
}
func userOK(v string) bool {
	v = strings.ToLower(v)
	for _, sample := range []string{"root", "git", "ubuntu", "admin", "shared", "public", "guest", "default", "user", "username", "yourname", "example", "runner", "node", "vscode", "codespace"} {
		if v == sample {
			return false
		}
	}
	return v != "" && !strings.HasPrefix(v, ".")
}
func hostOK(v string) bool {
	v = strings.ToLower(v)
	if _, err := netip.ParseAddr(v); err == nil {
		return false
	}
	for _, host := range []string{"github.com", "gitlab.com", "bitbucket.org", "localhost", "host", "hostname", "example.com", "example.net", "example.org", "example", "server"} {
		if v == host {
			return false
		}
	}
	return len(v) >= 2 && !versionPattern.MatchString(v) && !strings.HasSuffix(v, ".test") && !strings.HasSuffix(v, ".invalid") && !strings.HasSuffix(v, ".example")
}

var excludedIPs = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "100::/64"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

func publicIP(v string) bool {
	a, err := netip.ParseAddr(v)
	if err != nil || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	a = a.Unmap()
	for _, p := range excludedIPs {
		if p.Contains(a) {
			return false
		}
	}
	for _, dns := range []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9", "208.67.222.222", "208.67.220.220", "223.5.5.5", "223.6.6.6", "114.114.114.114", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if v == dns {
			return false
		}
	}
	return true
}
func ipAt(text string, a, _ int) bool {
	before := strings.ToLower(strings.TrimRight(text[max(0, a-24):a], " \t:=/"))
	for _, label := range []string{"version", "chrome", "firefox", "safari", "edge", "v"} {
		if strings.HasSuffix(before, label) {
			return false
		}
	}
	return true
}
func bucketOK(v string) bool {
	v = strings.ToLower(v)
	return len(v) >= 3 && v != "my-bucket" && v != "your-bucket" && v != "example-bucket" && v != "bucket-name" && v != "mybucket" && v != "example"
}

var secretNameSeparators = strings.NewReplacer("_", "", "-", "")

func secretName(key string) bool {
	lower := strings.ToLower(secretNameSeparators.Replace(key))
	for _, word := range []string{"password", "passwd", "secret", "apikey", "accesskey", "privatekey", "token", "credential", "authorization", "cookie"} {
		if strings.Contains(lower, word) && !strings.Contains(lower, "path") && !strings.Contains(lower, "file") && !strings.Contains(lower, "max_token") && !strings.Contains(lower, "maxtoken") {
			return true
		}
	}
	return strings.HasSuffix(key, "_KEY") || strings.HasSuffix(key, "_PASS")
}

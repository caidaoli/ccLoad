package sql

import "strings"

// RebindPostgres 将 ? 占位符转换为 PostgreSQL 的 $1,$2,...。
// 跳过单引号字符串字面量内的问号；不处理美元引用字符串（业务 SQL 不使用）。
func RebindPostgres(query string) string {
	if query == "" || !strings.Contains(query, "?") {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 16)
	arg := 0
	inString := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if inString {
			b.WriteByte(ch)
			if ch == '\'' {
				// SQL 转义 ''
				if i+1 < len(query) && query[i+1] == '\'' {
					b.WriteByte(query[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		}
		if ch == '\'' {
			inString = true
			b.WriteByte(ch)
			continue
		}
		if ch == '?' {
			// 避免把 ::type? 或标识符里的 ? 误伤；业务只用独立 ?
			arg++
			b.WriteByte('$')
			b.WriteString(itoa(arg))
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

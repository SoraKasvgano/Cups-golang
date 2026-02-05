package web

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type TemplateContext struct {
	vars   map[string]string
	arrays map[string][]string
}

func NewTemplateContext() *TemplateContext {
	return &TemplateContext{
		vars:   map[string]string{},
		arrays: map[string][]string{},
	}
}

func (c *TemplateContext) SetVar(name, value string) {
	c.vars[name] = value
}

func (c *TemplateContext) SetArray(name string, values []string) {
	c.arrays[name] = values
}

func (c *TemplateContext) getVar(name string) (string, bool) {
	v, ok := c.vars[name]
	return v, ok
}

func (c *TemplateContext) getArray(name string, idx int) (string, bool) {
	arr, ok := c.arrays[name]
	if !ok || idx < 0 || idx >= len(arr) {
		return "", false
	}
	return arr[idx], true
}

func (c *TemplateContext) size(name string) int {
	arr, ok := c.arrays[name]
	if !ok {
		return 0
	}
	return len(arr)
}

type cupsTemplate struct {
	data []byte
}

func loadCupsTemplate(fsys fs.FS, name string) (*cupsTemplate, error) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	return &cupsTemplate{data: b}, nil
}

func (t *cupsTemplate) Render(ctx *TemplateContext, element int) (string, error) {
	if t == nil {
		return "", errors.New("nil template")
	}
	in := bytes.NewReader(t.data)
	var out bytes.Buffer
	if err := cgiCopy(&out, in, ctx, element, 0); err != nil {
		return "", err
	}
	return out.String(), nil
}

func cgiCopy(out io.Writer, in *bytes.Reader, ctx *TemplateContext, element int, term byte) error {
	for {
		ch, err := in.ReadByte()
		if err == io.EOF {
			if term != 0 {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if term != 0 && ch == term {
			return nil
		}
		switch ch {
		case '{':
			if err := handleVariable(out, in, ctx, element); err != nil {
				return err
			}
		case '\\':
			escaped, err := in.ReadByte()
			if err != nil {
				return err
			}
			if out != nil {
				_, _ = out.Write([]byte{escaped})
			}
		default:
			if out != nil {
				_, _ = out.Write([]byte{ch})
			}
		}
	}
}

func handleVariable(out io.Writer, in *bytes.Reader, ctx *TemplateContext, element int) error {
	name, op, compare, err := parseVariable(in, element, ctx)
	if err != nil {
		return err
	}
	if op == '{' && name == "" {
		if out != nil {
			_, _ = out.Write([]byte{'{'})
		}
		return nil
	}

	if strings.HasPrefix(name, "[") {
		loopName := strings.TrimPrefix(name, "[")
		count := 0
		if loopName != "" && loopName[0] >= '0' && loopName[0] <= '9' {
			count, _ = strconv.Atoi(loopName)
		} else {
			count = ctx.size(loopName)
		}
		pos, _ := in.Seek(0, io.SeekCurrent)
		if count > 0 {
			for i := 0; i < count; i++ {
				if i > 0 {
					_, _ = in.Seek(pos, io.SeekStart)
				}
				if err := cgiCopy(out, in, ctx, i, '}'); err != nil {
					return err
				}
			}
		} else {
			if err := cgiCopy(nil, in, ctx, 0, '}'); err != nil {
				return err
			}
		}
		return nil
	}

	uriencode := strings.HasPrefix(name, "%")
	if uriencode {
		name = strings.TrimPrefix(name, "%")
	}

	value, found := resolveValue(ctx, name, element)
	compareValue := value
	if !found && !strings.HasPrefix(name, "?") && !strings.HasPrefix(name, "#") && !strings.HasPrefix(name, "$") && !strings.HasPrefix(name, "[") {
		compareValue = "{" + name + "}"
	}

	if op == 0 {
		if out != nil {
			if !found && !strings.HasPrefix(name, "?") && !strings.HasPrefix(name, "#") && !strings.HasPrefix(name, "$") && !strings.HasPrefix(name, "[") {
				value = "{" + name + "}"
			}
			if uriencode {
				_, _ = out.Write([]byte(uriEncode(value)))
			} else if strings.EqualFold(name, "?cupsdconf_default") {
				_, _ = out.Write([]byte(value))
			} else {
				_, _ = out.Write([]byte(htmlEscape(value)))
			}
		}
		return nil
	}

	// Evaluate condition
	var result bool
	switch op {
	case '?':
		result = compareValue != ""
	case '<', '>', '=', '!', '~':
		result = compareValues(op, compareValue, compare)
	default:
		result = false
	}

	if result {
		if err := cgiCopy(out, in, ctx, element, ':'); err != nil {
			return err
		}
		return cgiCopy(nil, in, ctx, element, '}')
	}
	if err := cgiCopy(nil, in, ctx, element, ':'); err != nil {
		return err
	}
	return cgiCopy(out, in, ctx, element, '}')
}

func parseVariable(in *bytes.Reader, element int, ctx *TemplateContext) (string, byte, string, error) {
	var nameBuf bytes.Buffer
	ch, err := in.ReadByte()
	if err != nil {
		return "", 0, "", err
	}
	// Handle lone "{ " sequence
	for {
		if bytes.ContainsAny([]byte{ch}, "}]<>=!~ \t\n") {
			break
		}
		if ch == '%' && nameBuf.Len() == 0 {
			nameBuf.WriteByte(ch)
			ch, err = in.ReadByte()
			if err != nil {
				return "", 0, "", err
			}
			continue
		}
		if ch == '?' && nameBuf.Len() > 0 {
			break
		}
		nameBuf.WriteByte(ch)
		ch, err = in.ReadByte()
		if err != nil {
			return "", 0, "", err
		}
	}

	name := nameBuf.String()
	if name == "" && (ch == ' ' || ch == '\t' || ch == '\n') {
		// Literal "{" - put the whitespace back for normal processing.
		_ = in.UnreadByte()
		return "", '{', "", nil
	}

	if ch == '}' {
		return name, 0, "", nil
	}

	op := ch
	if op == '?' {
		return name, op, "", nil
	}
	if op == '<' || op == '>' || op == '=' || op == '!' || op == '~' {
		compare, err := parseCompare(in, element, ctx)
		return name, op, compare, err
	}

	return name, 0, "", nil
}

func parseCompare(in *bytes.Reader, element int, ctx *TemplateContext) (string, error) {
	var buf bytes.Buffer
	for {
		ch, err := in.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '?' {
			break
		}
		if ch == '#' {
			buf.WriteString(strconv.Itoa(element + 1))
			continue
		}
		if ch == '{' {
			inner, err := parseInner(in, element, ctx)
			if err != nil {
				return "", err
			}
			buf.WriteString(inner)
			continue
		}
		if ch == '\\' {
			escaped, err := in.ReadByte()
			if err != nil {
				return "", err
			}
			buf.WriteByte(escaped)
			continue
		}
		buf.WriteByte(ch)
	}
	return buf.String(), nil
}

func parseInner(in *bytes.Reader, element int, ctx *TemplateContext) (string, error) {
	var innerBuf bytes.Buffer
	for {
		ch, err := in.ReadByte()
		if err != nil {
			return "", err
		}
		if ch == '}' {
			break
		}
		innerBuf.WriteByte(ch)
	}
	inner := innerBuf.String()
	if strings.HasPrefix(inner, "#") {
		return strconv.Itoa(ctx.size(strings.TrimPrefix(inner, "#"))), nil
	}
	val, _ := resolveValue(ctx, inner, element)
	return val, nil
}

func resolveValue(ctx *TemplateContext, name string, element int) (string, bool) {
	if strings.HasPrefix(name, "?") {
		name = strings.TrimPrefix(name, "?")
		if strings.Contains(name, "-") {
			base, idx := splitIndex(name)
			if idx >= 0 {
				if v, ok := ctx.getArray(base, idx); ok {
					return v, true
				}
				if v, ok := ctx.getVar(base); ok {
					return v, true
				}
				return "", false
			}
		}
		if v, ok := ctx.getArray(name, element); ok {
			return v, true
		}
		if v, ok := ctx.getVar(name); ok {
			return v, true
		}
		return "", false
	}
	if strings.HasPrefix(name, "#") {
		base := strings.TrimPrefix(name, "#")
		if base == "" {
			return strconv.Itoa(element + 1), true
		}
		return strconv.Itoa(ctx.size(base)), true
	}
	if strings.HasPrefix(name, "[") {
		return "", false
	}
	if strings.HasPrefix(name, "$") {
		return "", false
	}
	if strings.Contains(name, "-") {
		base, idx := splitIndex(name)
		if idx >= 0 {
			if arr, ok := ctx.arrays[base]; ok {
				if idx == 0 && len(arr) > 1 && element >= 0 && element < len(arr) {
					return arr[element], true
				}
				if idx >= 0 && idx < len(arr) {
					return arr[idx], true
				}
			}
			return "", false
		}
	}
	if v, ok := ctx.getArray(name, element); ok {
		return v, true
	}
	if v, ok := ctx.getVar(name); ok {
		return v, true
	}
	return "", false
}

func splitIndex(name string) (string, int) {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return name, -1
	}
	last := parts[len(parts)-1]
	if last == "" {
		return name, -1
	}
	n, err := strconv.Atoi(last)
	if err != nil {
		return name, -1
	}
	return strings.Join(parts[:len(parts)-1], "-"), n - 1
}

func compareValues(op byte, left, right string) bool {
	switch op {
	case '<':
		return strings.Compare(strings.ToLower(left), strings.ToLower(right)) < 0
	case '>':
		return strings.Compare(strings.ToLower(left), strings.ToLower(right)) > 0
	case '=':
		return strings.EqualFold(left, right)
	case '!':
		return !strings.EqualFold(left, right)
	case '~':
		re, err := regexp.Compile("(?i)" + right)
		if err != nil {
			return false
		}
		return re.MatchString(left)
	default:
		return false
	}
}

func htmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#39;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func uriEncode(s string) string {
	return url.PathEscape(s)
}

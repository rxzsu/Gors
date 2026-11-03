// Package diagnostic renders compiler errors with source excerpts.
package diagnostic

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"gors/internal/parser"
	"gors/internal/sema"
	"gors/internal/token"
)

// Format includes locations, source lines, related operations, and advice.
// Columns follow the lexer's one-based rune positions, including CRLF input.
func Format(path, source string, err error) string {
	if e, ok := err.(interface {
		SourceDiagnostic() (string, string, error)
	}); ok {
		p, s, inner := e.SourceDiagnostic()
		return Format(p, s, inner)
	}
	var pos token.Pos
	var message, hint string
	var notes []sema.Note
	switch e := err.(type) {
	case *sema.Error:
		pos, message, hint, notes = e.Pos, "error["+e.Code+"]: "+e.Msg, e.Hint, e.Notes
	case *parser.Error:
		pos, message = e.Pos, "error: "+e.Msg
	default:
		return fmt.Sprintf("%s: %s\n", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n")
	var out strings.Builder
	fmt.Fprintf(&out, "%s:%d:%d: %s\n", path, pos.Line, pos.Col, message)
	excerpt := func(p token.Pos) {
		if p.Line < 1 || p.Line > len(lines) || p.Col < 1 {
			return
		}
		runes := []rune(lines[p.Line-1])
		col := p.Col - 1
		if col > len(runes) {
			col = len(runes)
		}
		width := 1
		if col < len(runes) && (unicode.IsLetter(runes[col]) || runes[col] == '_') {
			for col+width < len(runes) && (unicode.IsLetter(runes[col+width]) || unicode.IsDigit(runes[col+width]) || runes[col+width] == '_') {
				width++
			}
		}
		// Preserve tab alignment in both the source and the marker prefix.
		var prefix strings.Builder
		for _, r := range runes[:col] {
			if r == '\t' {
				prefix.WriteRune('\t')
			} else {
				prefix.WriteByte(' ')
			}
		}
		gutter := len(strconv.Itoa(len(lines)))
		fmt.Fprintf(&out, " %*d | %s\n %s | %s%s\n", gutter, p.Line, lines[p.Line-1], strings.Repeat(" ", gutter), prefix.String(), strings.Repeat("^", width))
	}
	excerpt(pos)
	for _, note := range notes {
		fmt.Fprintf(&out, "note: %s (%s:%d:%d)\n", note.Msg, path, note.Pos.Line, note.Pos.Col)
		excerpt(note.Pos)
	}
	if hint != "" {
		fmt.Fprintf(&out, "help: %s\n", hint)
	}
	return out.String()
}

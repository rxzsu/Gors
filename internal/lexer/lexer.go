// Package lexer turns GoRs source text into a stream of tokens.
package lexer

import (
	"strings"
	"unicode"

	"gors/internal/token"
)

// Lexer scans source code.
type Lexer struct {
	src  []rune
	pos  int
	line int
	col  int
}

// New creates a lexer for src.
func New(src string) *Lexer {
	// Normalize CRLF so positions stay stable.
	src = strings.ReplaceAll(src, "\r\n", "\n")
	return &Lexer{src: []rune(src), line: 1, col: 1}
}

func (l *Lexer) peek(offset int) rune {
	i := l.pos + offset
	if i < 0 || i >= len(l.src) {
		return 0
	}
	return l.src[i]
}

func (l *Lexer) advance() rune {
	if l.pos >= len(l.src) {
		return 0
	}
	ch := l.src[l.pos]
	l.pos++
	if ch == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return ch
}

func (l *Lexer) skipSpaceAndComments() {
	for {
		ch := l.peek(0)
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			l.advance()
		case ch == '/' && l.peek(1) == '/':
			for l.peek(0) != '\n' && l.peek(0) != 0 {
				l.advance()
			}
		case ch == '/' && l.peek(1) == '*':
			l.advance()
			l.advance()
			for l.peek(0) != 0 {
				if l.peek(0) == '*' && l.peek(1) == '/' {
					l.advance()
					l.advance()
					break
				}
				l.advance()
			}
		default:
			return
		}
	}
}

func isIdentStart(ch rune) bool {
	return ch == '_' || unicode.IsLetter(ch)
}

func isIdentPart(ch rune) bool {
	return ch == '_' || unicode.IsLetter(ch) || unicode.IsDigit(ch)
}

// Tokens scans the whole input.
func (l *Lexer) Tokens() []token.Token {
	var out []token.Token
	for {
		l.skipSpaceAndComments()
		startLine, startCol := l.line, l.col
		ch := l.peek(0)
		if ch == 0 {
			out = append(out, token.Token{Type: token.EOF, Pos: token.Pos{Line: startLine, Col: startCol}})
			return out
		}

		switch {
		case isIdentStart(ch):
			var sb strings.Builder
			for isIdentPart(l.peek(0)) {
				sb.WriteRune(l.advance())
			}
			lit := sb.String()
			out = append(out, token.Token{
				Type: token.LookupIdent(lit),
				Lit:  lit,
				Pos:  token.Pos{Line: startLine, Col: startCol},
			})

		case unicode.IsDigit(ch):
			var sb strings.Builder
			isFloat := false
			for unicode.IsDigit(l.peek(0)) || l.peek(0) == '_' {
				c := l.advance()
				if c != '_' {
					sb.WriteRune(c)
				}
			}
			if l.peek(0) == '.' && unicode.IsDigit(l.peek(1)) {
				isFloat = true
				sb.WriteRune(l.advance())
				for unicode.IsDigit(l.peek(0)) || l.peek(0) == '_' {
					c := l.advance()
					if c != '_' {
						sb.WriteRune(c)
					}
				}
			}
			tt := token.INT
			if isFloat {
				tt = token.FLOAT
			}
			out = append(out, token.Token{Type: tt, Lit: sb.String(), Pos: token.Pos{Line: startLine, Col: startCol}})

		case ch == '"':
			l.advance()
			var sb strings.Builder
			for {
				c := l.peek(0)
				if c == 0 || c == '\n' {
					out = append(out, token.Token{Type: token.ILLEGAL, Lit: "unterminated string", Pos: token.Pos{Line: startLine, Col: startCol}})
					break
				}
				if c == '"' {
					l.advance()
					out = append(out, token.Token{Type: token.STRING, Lit: sb.String(), Pos: token.Pos{Line: startLine, Col: startCol}})
					break
				}
				if c == '\\' {
					l.advance()
					esc := l.advance()
					switch esc {
					case 'n':
						sb.WriteRune('\n')
					case 't':
						sb.WriteRune('\t')
					case 'r':
						sb.WriteRune('\r')
					case '\\':
						sb.WriteRune('\\')
					case '"':
						sb.WriteRune('"')
					case '0':
						sb.WriteRune(0)
					default:
						sb.WriteRune('\\')
						sb.WriteRune(esc)
					}
					continue
				}
				sb.WriteRune(l.advance())
			}

		default:
			c := l.advance()
			var tt token.Type
			lit := string(c)
			switch c {
			case '=':
				if l.peek(0) == '=' {
					l.advance()
					tt, lit = token.EQ, "=="
				} else {
					tt = token.ASSIGN
				}
			case '!':
				if l.peek(0) == '=' {
					l.advance()
					tt, lit = token.NE, "!="
				} else {
					tt = token.NOT
				}
			case '<':
				if l.peek(0) == '=' {
					l.advance()
					tt, lit = token.LE, "<="
				} else {
					tt = token.LT
				}
			case '>':
				if l.peek(0) == '=' {
					l.advance()
					tt, lit = token.GE, ">="
				} else {
					tt = token.GT
				}
			case '&':
				if l.peek(0) == '&' {
					l.advance()
					tt, lit = token.AND, "&&"
				} else {
					tt = token.AMP
				}
			case '|':
				if l.peek(0) == '|' {
					l.advance()
					tt, lit = token.OR, "||"
				} else {
					tt = token.ILLEGAL
				}
			case '-':
				if l.peek(0) == '>' {
					l.advance()
					tt, lit = token.ARROW, "->"
				} else {
					tt = token.MINUS
				}
			case '+':
				tt = token.PLUS
			case '*':
				tt = token.STAR
			case '/':
				tt = token.SLASH
			case '%':
				tt = token.PERCENT
			case '(':
				tt = token.LPAREN
			case ')':
				tt = token.RPAREN
			case '{':
				tt = token.LBRACE
			case '}':
				tt = token.RBRACE
			case '[':
				tt = token.LBRACK
			case ']':
				tt = token.RBRACK
			case ',':
				tt = token.COMMA
			case ':':
				tt = token.COLON
			case '.':
				tt = token.DOT
			case ';':
				tt = token.SEMI
			case '?':
				tt = token.QUESTION
			default:
				tt = token.ILLEGAL
			}
			out = append(out, token.Token{Type: tt, Lit: lit, Pos: token.Pos{Line: startLine, Col: startCol}})
		}
	}
}

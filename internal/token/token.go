// Package token defines the lexical tokens of the GoRs language.
package token

// Type is a token kind.
type Type string

const (
	ILLEGAL Type = "ILLEGAL"
	EOF     Type = "EOF"
	IDENT   Type = "IDENT"
	INT     Type = "INT"
	FLOAT   Type = "FLOAT"
	STRING  Type = "STRING"

	ASSIGN  Type = "="
	PLUS    Type = "+"
	MINUS   Type = "-"
	STAR    Type = "*"
	SLASH   Type = "/"
	PERCENT Type = "%"

	EQ  Type = "=="
	NE  Type = "!="
	LT  Type = "<"
	GT  Type = ">"
	LE  Type = "<="
	GE  Type = ">="
	AND Type = "&&"
	OR  Type = "||"
	NOT Type = "!"
	AMP Type = "&"

	ARROW Type = "->"

	LPAREN Type = "("
	RPAREN Type = ")"
	LBRACE Type = "{"
	RBRACE Type = "}"
	LBRACK Type = "["
	RBRACK Type = "]"
	COMMA  Type = ","
	COLON  Type = ":"
	DOT    Type = "."
	SEMI   Type = ";"

	FN     Type = "fn"
	LET    Type = "let"
	MUT    Type = "mut"
	IF     Type = "if"
	ELSE   Type = "else"
	WHILE  Type = "while"
	RETURN Type = "return"
	STRUCT Type = "struct"
	TRUE   Type = "true"
	FALSE  Type = "false"
)

// Pos is a source position (1-based line and column).
type Pos struct {
	Line int
	Col  int
}

// Token is a single lexical token.
type Token struct {
	Type Type
	Lit  string
	Pos  Pos
}

var keywords = map[string]Type{
	"fn":     FN,
	"let":    LET,
	"mut":    MUT,
	"if":     IF,
	"else":   ELSE,
	"while":  WHILE,
	"return": RETURN,
	"struct": STRUCT,
	"true":   TRUE,
	"false":  FALSE,
}

// LookupIdent maps an identifier to a keyword type when applicable.
func LookupIdent(ident string) Type {
	if t, ok := keywords[ident]; ok {
		return t
	}
	return IDENT
}

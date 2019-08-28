package function

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/hhatto/gocloc"
	"github.com/src-d/enry/v2"
	"github.com/src-d/go-mysql-server/sql"
)

var languages = gocloc.NewDefinedLanguages()

var errEmptyInputValues = errors.New("empty input values")

// LOC is a function that returns the count of different types of lines of code.
type LOC struct {
	Left  sql.Expression
	Right sql.Expression
}

// NewLOC creates a new LOC UDF.
func NewLOC(args ...sql.Expression) (sql.Expression, error) {
	if len(args) != 2 {
		return nil, sql.ErrInvalidArgumentNumber.New("2", len(args))
	}

	return &LOC{args[0], args[1]}, nil
}

// Resolved implements the Expression interface.
func (f *LOC) Resolved() bool {
	return f.Left.Resolved() && f.Right.Resolved()
}

func (f *LOC) String() string {
	return fmt.Sprintf("loc(%s, %s)", f.Left, f.Right)
}

// IsNullable implements the Expression interface.
func (f *LOC) IsNullable() bool {
	return f.Left.IsNullable() || f.Right.IsNullable()
}

// Type implements the Expression interface.
func (LOC) Type() sql.Type {
	return sql.JSON
}

// WithChildren implements the Expression interface.
func (f *LOC) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	return NewLOC(children...)
}

// LocFile is the result of the LOC function for each file.
type LocFile struct {
	Code     int32  `json:"Code"`
	Comments int32  `json:"Comment"`
	Blanks   int32  `json:"Blank"`
	Name     string `json:"Name"`
	Lang     string `json:"Language"`
}

// Eval implements the Expression interface.
func (f *LOC) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	span, ctx := ctx.Span("gitbase.LOC")
	defer span.Finish()
	path, blob, err := f.getInputValues(ctx, row)
	if err != nil {
		if err == errEmptyInputValues {
			return nil, nil
		}

		return nil, err
	}

	lang, err := f.getLanguage(path, blob)
	if err != nil {
		return nil, err
	}

	if lang == "" || languages.Langs[lang] == nil {
		return nil, nil
	}

	file := gocloc.AnalyzeReader(
		path,
		languages.Langs[lang],
		bytes.NewReader(blob), &gocloc.ClocOptions{},
	)

	return LocFile{
		Code:     file.Code,
		Comments: file.Comments,
		Blanks:   file.Blanks,
		Name:     file.Name,
		Lang:     file.Lang,
	}, nil
}

func (f *LOC) getInputValues(ctx *sql.Context, row sql.Row) (string, []byte, error) {
	left, err := f.Left.Eval(ctx, row)
	if err != nil {
		return "", nil, err
	}

	left, err = sql.Text.Convert(left)
	if err != nil {
		return "", nil, err
	}

	right, err := f.Right.Eval(ctx, row)
	if err != nil {
		return "", nil, err
	}

	right, err = sql.Blob.Convert(right)
	if err != nil {
		return "", nil, err
	}

	if right == nil {
		return "", nil, errEmptyInputValues
	}

	path, ok := left.(string)
	if !ok {
		return "", nil, errEmptyInputValues
	}

	blob, ok := right.([]byte)

	if !ok {
		return "", nil, errEmptyInputValues
	}

	if len(blob) == 0 || len(path) == 0 {
		return "", nil, errEmptyInputValues
	}

	return path, blob, nil
}

func (f *LOC) getLanguage(path string, blob []byte) (string, error) {
	hash := languageHash(path, blob)

	value, err := languageCache.Get(hash)
	if err == nil {
		return value.(string), nil
	}

	lang := enry.GetLanguage(path, blob)
	if len(blob) > 0 {
		if err := languageCache.Put(hash, lang); err != nil {
			return "", err
		}
	}

	return lang, nil
}

// Children implements the Expression interface.
func (f *LOC) Children() []sql.Expression {
	if f.Right == nil {
		return []sql.Expression{f.Left}
	}

	return []sql.Expression{f.Left, f.Right}
}

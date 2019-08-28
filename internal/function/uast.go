package function

import (
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	bblfsh "github.com/bblfsh/go-client/v4"
	derrors "github.com/bblfsh/sdk/v3/driver/errors"
	"github.com/bblfsh/sdk/v3/uast"
	"github.com/bblfsh/sdk/v3/uast/nodes"
	"github.com/go-kit/kit/metrics/discard"
	"github.com/sirupsen/logrus"

	"github.com/src-d/go-mysql-server/sql"
	"github.com/src-d/go-mysql-server/sql/expression"
)

const (
	uastCacheSizeKey     = "GITBASE_UAST_CACHE_SIZE"
	defaultUASTCacheSize = 10000

	uastMaxBlobSizeKey     = "GITBASE_MAX_UAST_BLOB_SIZE"
	defaultUASTMaxBlobSize = 5 * 1024 * 1024 // 5MB
)

var (
	// UastHitCacheCounter describes a metric that accumulates number of hit cache uast queries monotonically.
	UastHitCacheCounter = discard.NewCounter()

	// UastMissCacheCounter describes a metric that accumulates number of miss cache uast queries monotonically.
	UastMissCacheCounter = discard.NewCounter()

	// UastQueryHistogram describes a uast queries latency.
	UastQueryHistogram = discard.NewHistogram()
)

func observeQuery(lang, xpath string, t time.Time) func(bool) {
	return func(ok bool) {
		UastQueryHistogram.With("lang", lang, "xpath", xpath, "duration", "seconds").Observe(time.Since(t).Seconds())
		if ok {
			UastHitCacheCounter.With("lang", lang, "xpath", xpath).Add(1)
		} else {
			UastMissCacheCounter.With("lang", lang, "xpath", xpath).Add(1)
		}
	}
}

var (
	uastmut         sync.Mutex
	uastCache       sql.KeyValueCache
	uastCacheSize   int
	uastMaxBlobSize int
)

func getUASTCache(ctx *sql.Context) sql.KeyValueCache {
	uastmut.Lock()
	defer uastmut.Unlock()
	if uastCache == nil {
		// Dispose function is ignored because the cache will never be disposed
		// until the program dies.
		uastCache, _ = ctx.Memory.NewLRUCache(uint(uastCacheSize))
	}

	return uastCache
}

func init() {
	s := os.Getenv(uastCacheSizeKey)
	size, err := strconv.Atoi(s)
	if err != nil || size <= 0 {
		size = defaultUASTCacheSize
	}

	uastCacheSize = size

	uastMaxBlobSize, err = strconv.Atoi(os.Getenv(uastMaxBlobSizeKey))
	if err != nil {
		uastMaxBlobSize = defaultUASTMaxBlobSize
	}
}

// uastFunc shouldn't be used as an sql.Expression itself.
// It's intended to be embedded in others UAST functions,
// like UAST and UASTMode.
type uastFunc struct {
	Mode  sql.Expression
	Blob  sql.Expression
	Lang  sql.Expression
	XPath sql.Expression

	h hash.Hash64
	m sync.Mutex
}

// IsNullable implements the Expression interface.
func (u *uastFunc) IsNullable() bool {
	return u.Blob.IsNullable() || u.Mode.IsNullable() ||
		(u.Lang != nil && u.Lang.IsNullable()) ||
		(u.XPath != nil && u.XPath.IsNullable())
}

// Resolved implements the Expression interface.
func (u *uastFunc) Resolved() bool {
	return u.Blob.Resolved() && u.Mode.Resolved() &&
		(u.Lang == nil || u.Lang.Resolved()) &&
		(u.XPath == nil || u.XPath.Resolved())
}

// Type implements the Expression interface.
func (u *uastFunc) Type() sql.Type {
	return sql.Blob
}

// Children implements the Expression interface.
func (u *uastFunc) Children() []sql.Expression {
	exprs := []sql.Expression{u.Blob, u.Mode}
	if u.Lang != nil {
		exprs = append(exprs, u.Lang)
	}
	if u.XPath != nil {
		exprs = append(exprs, u.XPath)
	}
	return exprs
}

// WithChildren implements the Expression interface.
func (u *uastFunc) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	expected := 2
	if u.Lang != nil {
		expected++
	}

	if u.XPath != nil {
		expected++
	}

	if len(children) != expected {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), expected)
	}

	blob := children[0]
	mode := children[1]
	var lang, xpath sql.Expression
	var idx = 2
	if u.Lang != nil {
		lang = children[idx]
		idx++
	}

	if u.XPath != nil {
		xpath = children[idx]
	}

	return &uastFunc{
		Mode:  mode,
		Blob:  blob,
		XPath: xpath,
		Lang:  lang,
		h:     newHash(),
	}, nil
}

// String implements the Expression interface.
func (u *uastFunc) String() string {
	panic("method String() shouldn't be called directly on an uastFunc")
}

// Eval implements the Expression interface.
func (u *uastFunc) Eval(ctx *sql.Context, row sql.Row) (out interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("uast: unknown error: %s", r)
		}
	}()

	span, ctx := ctx.Span("gitbase.UAST")
	defer span.Finish()

	m, err := exprToString(ctx, u.Mode, row)
	if err != nil {
		return nil, err
	}

	mode, err := bblfsh.ParseMode(m)
	if err != nil {
		return nil, err
	}

	blob, err := u.Blob.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	if blob == nil {
		return nil, nil
	}

	blob, err = sql.Blob.Convert(blob)
	if err != nil {
		return nil, err
	}

	bytes := blob.([]byte)
	if len(bytes) == 0 {
		return nil, nil
	}

	if uastMaxBlobSize >= 0 && len(bytes) > uastMaxBlobSize {
		logrus.WithFields(logrus.Fields{
			"max":  uastMaxBlobSize,
			"size": len(bytes),
		}).Warnf(
			"uast will be skipped, file is too big to send to bblfsh."+
				"This can be configured using %s environment variable",
			uastMaxBlobSizeKey,
		)

		ctx.Warn(
			0,
			"uast will be skipped, file is too big to send to bblfsh."+
				"This can be configured using %s environment variable",
			uastMaxBlobSizeKey,
		)
		return nil, nil
	}

	lang, err := exprToString(ctx, u.Lang, row)
	if err != nil {
		return nil, err
	}

	lang = strings.ToLower(lang)

	xpath, err := exprToString(ctx, u.XPath, row)
	if err != nil {
		return nil, err
	}

	return u.getUAST(ctx, bytes, lang, xpath, mode)
}

func (u *uastFunc) computeKey(mode, lang string, blob []byte) (uint64, error) {
	u.m.Lock()
	defer u.m.Unlock()

	return computeKey(u.h, mode, lang, blob)
}

func (u *uastFunc) getUAST(
	ctx *sql.Context,
	blob []byte,
	lang, xpath string,
	mode bblfsh.Mode,
) (interface{}, error) {
	finish := observeQuery(lang, xpath, time.Now())

	key, err := u.computeKey(mode.String(), lang, blob)
	if err != nil {
		return nil, err
	}

	uastCache := getUASTCache(ctx)

	var node nodes.Node
	value, err := uastCache.Get(key)
	cacheMiss := err != nil
	if !cacheMiss {
		node = value.(nodes.Node)
	} else {
		var err error
		node, err = getUASTFromBblfsh(ctx, blob, lang, xpath, mode)
		if err != nil {
			if ErrParseBlob.Is(err) || derrors.ErrSyntax.Is(err) {
				return nil, nil
			}

			return nil, err
		}

		if err := uastCache.Put(key, node); err != nil {
			return nil, err
		}
	}

	var nodeArray nodes.Array
	if xpath == "" {
		nodeArray = append(nodeArray, node)
	} else {
		var err error
		nodeArray, err = applyXpath(node, xpath)
		if err != nil {
			logrus.WithField("err", err).
				Errorf("unable to filter node using xpath: %s", xpath)
			return nil, nil
		}
	}

	result, err := marshalNodes(nodeArray)
	if err != nil {
		logrus.WithField("err", err).
			Error("unable to marshal UAST nodes")
		return nil, nil
	}

	finish(!cacheMiss)

	return result, nil
}

// UAST returns an array of UAST nodes as blobs.
type UAST struct {
	*uastFunc
}

// NewUAST creates a new UAST UDF.
func NewUAST(args ...sql.Expression) (sql.Expression, error) {
	var mode = expression.NewLiteral("semantic", sql.Text)
	var blob, lang, xpath sql.Expression

	switch len(args) {
	default:
		return nil, sql.ErrInvalidArgumentNumber.New("uast", "1, 2 or 3", len(args))
	case 3:
		xpath = args[2]
		fallthrough
	case 2:
		lang = args[1]
		fallthrough
	case 1:
		blob = args[0]
	}

	return &UAST{&uastFunc{
		Mode:  mode,
		Blob:  blob,
		Lang:  lang,
		XPath: xpath,
		h:     newHash(),
	}}, nil
}

// WithChildren implements the Expression interface.
func (u *UAST) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	expected := 1
	if u.Lang != nil {
		expected++
	}

	if u.XPath != nil {
		expected++
	}

	if len(children) != expected {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), expected)
	}

	return NewUAST(children...)
}

// Children implements the Expression interface.
func (u *UAST) Children() []sql.Expression {
	result := []sql.Expression{u.Blob}
	if u.Lang != nil {
		result = append(result, u.Lang)
	}
	if u.XPath != nil {
		result = append(result, u.XPath)
	}
	return result
}

// String implements the Expression interface.
func (u *UAST) String() string {
	if u.Lang != nil && u.XPath != nil {
		return fmt.Sprintf("uast(%s, %s, %s)", u.Blob, u.Lang, u.XPath)
	}

	if u.Lang != nil {
		return fmt.Sprintf("uast(%s, %s)", u.Blob, u.Lang)
	}

	return fmt.Sprintf("uast(%s)", u.Blob)
}

// UASTMode returns an array of UAST nodes as blobs.
type UASTMode struct {
	*uastFunc
}

// NewUASTMode creates a new UASTMode UDF.
func NewUASTMode(mode, blob, lang sql.Expression) sql.Expression {
	return &UASTMode{&uastFunc{
		Mode:  mode,
		Blob:  blob,
		Lang:  lang,
		XPath: nil,
		h:     newHash(),
	}}
}

// WithChildren implements the Expression interface.
func (u *UASTMode) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 3 {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), 3)
	}

	return NewUASTMode(children[0], children[1], children[2]), nil
}

// String implements the Expression interface.
func (u *UASTMode) String() string {
	return fmt.Sprintf("uast_mode(%s, %s, %s)", u.Mode, u.Blob, u.Lang)
}

// UASTXPath performs an XPath query over the given UAST nodes.
type UASTXPath struct {
	expression.BinaryExpression
}

// NewUASTXPath creates a new UASTXPath UDF.
func NewUASTXPath(uast, xpath sql.Expression) sql.Expression {
	return &UASTXPath{expression.BinaryExpression{Left: uast, Right: xpath}}
}

// Type implements the Expression interface.
func (UASTXPath) Type() sql.Type {
	return sql.Blob
}

// Eval implements the Expression interface.
func (f *UASTXPath) Eval(ctx *sql.Context, row sql.Row) (out interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("uastxpath: unknown error: %s", r)
		}
	}()

	span, ctx := ctx.Span("gitbase.UASTXPath")
	defer span.Finish()

	xpath, err := exprToString(ctx, f.Right, row)
	if err != nil {
		return nil, err
	}

	if xpath == "" {
		return nil, nil
	}

	left, err := f.Left.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	ns, err := getNodes(left)
	if err != nil {
		return nil, err
	}

	if ns == nil {
		return nil, nil
	}

	var filtered nodes.Array
	for _, n := range ns {
		partial, err := applyXpath(n, xpath)
		if err != nil {
			return nil, err
		}

		filtered = append(filtered, partial...)
	}

	return marshalNodes(filtered)
}

func (f UASTXPath) String() string {
	return fmt.Sprintf("uast_xpath(%s, %s)", f.Left, f.Right)
}

// WithChildren implements the Expression interface.
func (f *UASTXPath) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 2)
	}

	return NewUASTXPath(children[0], children[1]), nil
}

// UASTExtract extracts keys from an UAST.
type UASTExtract struct {
	expression.BinaryExpression
}

// NewUASTExtract creates a new UASTExtract UDF.
func NewUASTExtract(uast, key sql.Expression) sql.Expression {
	return &UASTExtract{expression.BinaryExpression{Left: uast, Right: key}}
}

// String implements the fmt.Stringer interface.
func (u *UASTExtract) String() string {
	return fmt.Sprintf("uast_extract(%s, %s)", u.Left, u.Right)
}

// Type implements the sql.Expression interface.
func (u *UASTExtract) Type() sql.Type {
	return sql.Array(sql.Text)
}

// Eval implements the sql.Expression interface.
func (u *UASTExtract) Eval(ctx *sql.Context, row sql.Row) (out interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("uast: unknown error: %s", r)
		}
	}()

	span, ctx := ctx.Span("gitbase.UASTExtract")
	defer span.Finish()

	left, err := u.Left.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	ns, err := getNodes(left)
	if err != nil {
		return nil, err
	}

	if ns == nil {
		return nil, nil
	}

	key, err := exprToString(ctx, u.Right, row)
	if err != nil {
		return nil, err
	}

	if key == "" {
		return nil, nil
	}

	extracted := []interface{}{}
	for _, n := range ns {
		props := extractProperties(n, key)
		if len(props) > 0 {
			extracted = append(extracted, props...)
		}
	}

	return extracted, nil
}

func extractProperties(n nodes.Node, key string) []interface{} {
	node, ok := n.(nodes.Object)
	if !ok {
		return nil
	}

	var extracted []interface{}
	if isCommonProp(key) {
		extracted = extractCommonProp(node, key)
	} else {
		extracted = extractAnyProp(node, key)
	}

	return extracted
}

func isCommonProp(key string) bool {
	return key == uast.KeyType || key == uast.KeyToken ||
		key == uast.KeyRoles || key == uast.KeyPos
}

func extractCommonProp(node nodes.Object, key string) []interface{} {
	var extracted []interface{}
	switch key {
	case uast.KeyType:
		t := uast.TypeOf(node)
		if t != "" {
			extracted = append(extracted, t)
		}
	case uast.KeyToken:
		t := uast.TokenOf(node)
		if t != "" {
			extracted = append(extracted, t)
		}
	case uast.KeyRoles:
		r := uast.RolesOf(node)
		if len(r) > 0 {
			roles := make([]interface{}, len(r))
			for i, role := range r {
				roles[i] = role.String()
			}

			extracted = append(extracted, roles...)
		}
	case uast.KeyPos:
		p := uast.PositionsOf(node)
		if p != nil {
			if s := posToString(p); s != "" {
				extracted = append(extracted, s)
			}
		}
	}

	return extracted
}

func posToString(pos uast.Positions) string {
	var b strings.Builder
	if data, err := json.Marshal(pos); err == nil {
		b.Write(data)
	}
	return b.String()
}

func extractAnyProp(node nodes.Object, key string) []interface{} {
	v, ok := node[key]
	if !ok || v == nil {
		return nil
	}

	if v.Kind().In(nodes.KindsValues) {
		value, err := valueToString(v.(nodes.Value))
		if err != nil {
			return nil
		}

		return []interface{}{value}
	}

	if v.Kind() == nodes.KindArray {
		values, err := valuesFromNodeArray(v.(nodes.Array))
		if err != nil {
			return nil
		}

		return values
	}

	return nil
}

func valuesFromNodeArray(arr nodes.Array) ([]interface{}, error) {
	var values []interface{}
	for _, n := range arr {
		if n.Kind().In(nodes.KindsValues) {
			s, err := valueToString(n.(nodes.Value))
			if err != nil {
				return nil, err
			}

			values = append(values, s)
		}
	}

	return values, nil
}

func valueToString(n nodes.Value) (interface{}, error) {
	return sql.Text.Convert(n.Native())
}

// WithChildren implements the Expression interface.
func (u *UASTExtract) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), 2)
	}

	return NewUASTExtract(children[0], children[1]), nil
}

// UASTChildren returns children from UAST nodes.
type UASTChildren struct {
	expression.UnaryExpression
}

// NewUASTChildren creates a new UASTExtract UDF.
func NewUASTChildren(uast sql.Expression) sql.Expression {
	return &UASTChildren{expression.UnaryExpression{Child: uast}}
}

// String implements the fmt.Stringer interface.
func (u *UASTChildren) String() string {
	return fmt.Sprintf("uast_children(%s)", u.Child)
}

// Type implements the sql.Expression interface.
func (u *UASTChildren) Type() sql.Type {
	return sql.Blob
}

// WithChildren implements the Expression interface.
func (u *UASTChildren) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(u, len(children), 1)
	}

	return NewUASTChildren(children[0]), nil
}

// Eval implements the sql.Expression interface.
func (u *UASTChildren) Eval(ctx *sql.Context, row sql.Row) (out interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("uast: unknown error: %s", r)
		}
	}()

	span, ctx := ctx.Span("gitbase.UASTChildren")
	defer span.Finish()

	child, err := u.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	nodes, err := getNodes(child)
	if err != nil {
		return nil, err
	}

	if nodes == nil {
		return nil, nil
	}

	children := flattenChildren(nodes)
	return marshalNodes(children)
}

func flattenChildren(arr nodes.Array) nodes.Array {
	var children nodes.Array
	for _, n := range arr {
		o, ok := n.(nodes.Object)
		if !ok {
			continue
		}

		c := getChildren(o)
		if len(c) > 0 {
			children = append(children, c...)
		}
	}

	return children
}

func getChildren(node nodes.Object) nodes.Array {
	var children nodes.Array
	for _, key := range node.Keys() {
		if isCommonProp(key) {
			continue
		}

		c, ok := node[key]
		if !ok {
			continue
		}

		switch c.Kind() {
		case nodes.KindObject:
			children = append(children, c)
		case nodes.KindArray:
			for _, n := range c.(nodes.Array) {
				if n.Kind() == nodes.KindObject {
					children = append(children, n)
				}
			}
		}
	}

	return children
}

// UASTImports finds the imports in UAST nodes.
type UASTImports struct {
	expression.UnaryExpression
}

// NewUASTImports creates a new UASTImports node.
func NewUASTImports(child sql.Expression) sql.Expression {
	return &UASTImports{expression.UnaryExpression{Child: child}}
}

// Type implements the sql.Expression interface.
func (f *UASTImports) Type() sql.Type {
	return sql.Array(sql.Array(sql.Text))
}

// IsNullable implements the sql.Expression interface.
func (f *UASTImports) IsNullable() bool { return true }

// Eval implements the sql.Expression interface.
func (f *UASTImports) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	span, ctx := ctx.Span("function.UASTImports")
	defer span.Finish()

	child, err := f.Child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}

	nodes, err := getNodes(child)
	if err != nil {
		return nil, err
	}

	if nodes == nil {
		return nil, nil
	}

	var result = make([]interface{}, nodes.Size())
	for i := 0; i < nodes.Size(); i++ {
		node := nodes.ValueAt(i)
		imports := uast.AllImportPaths(node)
		nodeImports := make([]interface{}, len(imports))
		for j, imp := range imports {
			nodeImports[j] = imp
		}
		result[i] = nodeImports
	}

	return result, nil
}

func (f *UASTImports) String() string {
	return fmt.Sprintf("uast_imports(%s)", f.Child)
}

// Children implements the sql.Expression interface.
func (f *UASTImports) Children() []sql.Expression { return []sql.Expression{f.Child} }

// WithChildren implements the sql.Expression interface.
func (f *UASTImports) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(f, len(children), 1)
	}

	return NewUASTImports(children[0]), nil
}

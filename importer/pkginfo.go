package importer

// TODO(gri): absorb this into go/types.

import (
	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"go/ast"
)

// PackageInfo holds the ASTs and facts derived by the type-checker
// for a single package.
//
// Not mutated once constructed.
//
type PackageInfo struct {
	Pkg   *types.Package
	Files []*ast.File // abstract syntax for the package's files

	// Type-checker deductions.
	types     map[ast.Expr]types.Type        // inferred types of expressions
	constants map[ast.Expr]exact.Value       // values of constant expressions
	idents    map[*ast.Ident]types.Object    // resoved objects for named entities
	typecases map[*ast.CaseClause]*types.Var // implicit vars for single-type typecases
}

// TypeOf returns the type of expression e.
// Precondition: e belongs to the package's ASTs.
//
func (info *PackageInfo) TypeOf(e ast.Expr) types.Type {
	// For Ident, b.types may be more specific than
	// b.obj(id.(*ast.Ident)).GetType(),
	// e.g. in the case of typeswitch.
	if t, ok := info.types[e]; ok {
		return t
	}
	// The typechecker doesn't notify us of all Idents,
	// e.g. s.Key and s.Value in a RangeStmt.
	// So we have this fallback.
	// TODO(gri): This is a typechecker bug.  When fixed,
	// eliminate this case and panic.
	if id, ok := e.(*ast.Ident); ok {
		return info.ObjectOf(id).Type()
	}
	panic("no type for expression")
}

// ValueOf returns the value of expression e if it is a constant, nil
// otherwise.
//
func (info *PackageInfo) ValueOf(e ast.Expr) exact.Value {
	return info.constants[e]
}

// ObjectOf returns the typechecker object denoted by the specified id.
// Precondition: id belongs to the package's ASTs.
//
func (info *PackageInfo) ObjectOf(id *ast.Ident) types.Object {
	return info.idents[id]
}

// IsType returns true iff expression e denotes a type.
// Precondition: e belongs to the package's ASTs.
// e must be a true expression, not a KeyValueExpr, or an Ident
// appearing in a SelectorExpr or declaration.
//
func (info *PackageInfo) IsType(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr: // pkg.Type
		if obj := info.IsPackageRef(e); obj != nil {
			_, isType := obj.(*types.TypeName)
			return isType
		}
	case *ast.StarExpr: // *T
		return info.IsType(e.X)
	case *ast.Ident:
		_, isType := info.ObjectOf(e).(*types.TypeName)
		return isType
	case *ast.ArrayType, *ast.StructType, *ast.FuncType, *ast.InterfaceType, *ast.MapType, *ast.ChanType:
		return true
	case *ast.ParenExpr:
		return info.IsType(e.X)
	}
	return false
}

// IsPackageRef returns the identity of the object if sel is a
// package-qualified reference to a named const, var, func or type.
// Otherwise it returns nil.
// Precondition: sel belongs to the package's ASTs.
//
func (info *PackageInfo) IsPackageRef(sel *ast.SelectorExpr) types.Object {
	if id, ok := sel.X.(*ast.Ident); ok {
		if pkg, ok := info.ObjectOf(id).(*types.Package); ok {
			return pkg.Scope().Lookup(nil, sel.Sel.Name)
		}
	}
	return nil
}

// TypeCaseVar returns the implicit variable created by a single-type
// case clause in a type switch, or nil if not found.
//
func (info *PackageInfo) TypeCaseVar(cc *ast.CaseClause) *types.Var {
	return info.typecases[cc]
}

var (
	tEface      = new(types.Interface)
	tComplex64  = types.Typ[types.Complex64]
	tComplex128 = types.Typ[types.Complex128]
	tFloat32    = types.Typ[types.Float32]
	tFloat64    = types.Typ[types.Float64]
)

// BuiltinCallSignature returns a new Signature describing the
// effective type of a builtin operator for the particular call e.
//
// This requires ad-hoc typing rules for all variadic (append, print,
// println) and polymorphic (append, copy, delete, close) built-ins.
// This logic could be part of the typechecker, and should arguably
// be moved there and made accessible via an additional types.Context
// callback.
//
// The returned Signature is degenerate and only intended for use by
// emitCallArgs.
//
func (info *PackageInfo) BuiltinCallSignature(e *ast.CallExpr) *types.Signature {
	var params []*types.Var
	var isVariadic bool

	switch builtin := unparen(e.Fun).(*ast.Ident).Name; builtin {
	case "append":
		var t0, t1 types.Type
		t0 = info.TypeOf(e) // infer arg[0] type from result type
		if e.Ellipsis != 0 {
			// append([]T, []T) []T
			// append([]byte, string) []byte
			t1 = info.TypeOf(e.Args[1]) // no conversion
		} else {
			// append([]T, ...T) []T
			t1 = t0.Underlying().(*types.Slice).Elem()
			isVariadic = true
		}
		params = append(params,
			types.NewVar(nil, "", t0),
			types.NewVar(nil, "", t1))

	case "print", "println": // print{,ln}(any, ...interface{})
		isVariadic = true
		// Note, arg0 may have any type, not necessarily tEface.
		params = append(params,
			types.NewVar(nil, "", info.TypeOf(e.Args[0])),
			types.NewVar(nil, "", tEface))

	case "close":
		params = append(params, types.NewVar(nil, "", info.TypeOf(e.Args[0])))

	case "copy":
		// copy([]T, []T) int
		// Infer arg types from each other.  Sleazy.
		var st *types.Slice
		if t, ok := info.TypeOf(e.Args[0]).Underlying().(*types.Slice); ok {
			st = t
		} else if t, ok := info.TypeOf(e.Args[1]).Underlying().(*types.Slice); ok {
			st = t
		} else {
			panic("cannot infer types in call to copy()")
		}
		stvar := types.NewVar(nil, "", st)
		params = append(params, stvar, stvar)

	case "delete":
		// delete(map[K]V, K)
		tmap := info.TypeOf(e.Args[0])
		tkey := tmap.Underlying().(*types.Map).Key()
		params = append(params,
			types.NewVar(nil, "", tmap),
			types.NewVar(nil, "", tkey))

	case "len", "cap":
		params = append(params, types.NewVar(nil, "", info.TypeOf(e.Args[0])))

	case "real", "imag":
		// Reverse conversion to "complex" case below.
		var argType types.Type
		switch info.TypeOf(e).(*types.Basic).Kind() {
		case types.UntypedFloat:
			argType = types.Typ[types.UntypedComplex]
		case types.Float64:
			argType = tComplex128
		case types.Float32:
			argType = tComplex64
		default:
			unreachable()
		}
		params = append(params, types.NewVar(nil, "", argType))

	case "complex":
		var argType types.Type
		switch info.TypeOf(e).(*types.Basic).Kind() {
		case types.UntypedComplex:
			argType = types.Typ[types.UntypedFloat]
		case types.Complex128:
			argType = tFloat64
		case types.Complex64:
			argType = tFloat32
		default:
			unreachable()
		}
		v := types.NewVar(nil, "", argType)
		params = append(params, v, v)

	case "panic":
		params = append(params, types.NewVar(nil, "", tEface))

	case "recover":
		// no params

	default:
		panic("unknown builtin: " + builtin)
	}

	return types.NewSignature(nil, types.NewTuple(params...), nil, isVariadic)
}
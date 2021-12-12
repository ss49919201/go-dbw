package dbw

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"

	"gorm.io/gorm/clause"
)

// OpType defines a set of database operation types
type OpType int

const (
	UnknownOp OpType = 0
	CreateOp  OpType = 1
	UpdateOp  OpType = 2
	DeleteOp  OpType = 3
)

// VetForWriter provides an interface that Create and Update can use to vet the
// resource before before writing it to the db.  For optType == UpdateOp,
// options WithFieldMaskPath and WithNullPaths are supported.  For optType ==
// CreateOp, no options are supported
type VetForWriter interface {
	VetForWrite(ctx context.Context, r Reader, opType OpType, opt ...Option) error
}

var nonCreateFields atomic.Value

// InitNonCreatableFields sets the fields which are not setable using
// via RW.Create(...)
func InitNonCreatableFields(fields []string) {
	m := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		m[f] = struct{}{}
	}
	nonCreateFields.Store(m)
}

// NonCreatableFields returns the current set of fields which are not setable using
// via RW.Create(...)
func NonCreatableFields() []string {
	m := nonCreateFields.Load()
	if m == nil {
		return []string{}
	}
	fields := make([]string, 0, len(m.(map[string]struct{})))
	for f := range m.(map[string]struct{}) {
		fields = append(fields, f)
	}
	return fields
}

// Create a resource in the db with options: WithDebug, WithLookup,
// WithReturnRowsAffected, OnConflict, WithBeforeWrite, WithAfterWrite,
// WithVersion, WithTable, and WithWhere.
//
// OnConflict specifies alternative actions to take when an insert results in a
// unique constraint or exclusion constraint error. If WithVersion is used with
// OnConflict, then the update for on conflict will include the version number,
// which basically makes the update use optimistic locking and the update will
// only succeed if the existing rows version matches the WithVersion option.
// Zero is not a valid value for the WithVersion option and will return an
// error. WithWhere allows specifying an additional constraint on the on
// conflict operation in addition to the on conflict target policy (columns or
// constraint).
func (rw *RW) Create(ctx context.Context, i interface{}, opt ...Option) error {
	const op = "dbw.Create"
	if rw.underlying == nil {
		return fmt.Errorf("%s: missing underlying db: %w", op, ErrInvalidParameter)
	}
	if isNil(i) {
		return fmt.Errorf("%s: missing interface: %w", op, ErrInvalidParameter)
	}
	if err := raiseErrorOnHooks(i); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	opts := GetOpts(opt...)

	// these fields should be nil, since they are not writeable and we want the
	// db to manage them
	setFieldsToNil(i, NonCreatableFields())

	if !opts.WithSkipVetForWrite {
		if vetter, ok := i.(VetForWriter); ok {
			if err := vetter.VetForWrite(ctx, rw, CreateOp); err != nil {
				return fmt.Errorf("%s: %w", op, err)
			}
		}
	}

	db := rw.underlying.wrapped.WithContext(ctx)
	if opts.WithOnConflict != nil {
		c := clause.OnConflict{}
		switch opts.WithOnConflict.Target.(type) {
		case Constraint:
			c.OnConstraint = string(opts.WithOnConflict.Target.(Constraint))
		case Columns:
			columns := make([]clause.Column, 0, len(opts.WithOnConflict.Target.(Columns)))
			for _, name := range opts.WithOnConflict.Target.(Columns) {
				columns = append(columns, clause.Column{Name: name})
			}
			c.Columns = columns
		default:
			return fmt.Errorf("%s: invalid conflict target %v: %w", op, reflect.TypeOf(opts.WithOnConflict.Target), ErrInvalidParameter)
		}

		switch opts.WithOnConflict.Action.(type) {
		case DoNothing:
			c.DoNothing = true
		case UpdateAll:
			c.UpdateAll = true
		case []ColumnValue:
			updates := opts.WithOnConflict.Action.([]ColumnValue)
			set := make(clause.Set, 0, len(updates))
			for _, s := range updates {
				// make sure it's not one of the std immutable columns
				if contains([]string{"createtime", "publicid"}, strings.ToLower(s.Column)) {
					return fmt.Errorf("%s: cannot do update on conflict for column %s: %w", op, s.Column, ErrInvalidParameter)
				}
				switch sv := s.Value.(type) {
				case column:
					set = append(set, sv.toAssignment(s.Column))
				case ExprValue:
					set = append(set, sv.toAssignment(s.Column))
				default:
					set = append(set, rawAssignment(s.Column, s.Value))
				}
			}
			c.DoUpdates = set
		default:
			return fmt.Errorf("%s: invalid conflict action %v: %w", op, reflect.TypeOf(opts.WithOnConflict.Action), ErrInvalidParameter)
		}
		if opts.WithVersion != nil || opts.WithWhereClause != "" {
			where, args, err := rw.whereClausesFromOpts(ctx, i, opts)
			if err != nil {
				return fmt.Errorf("%s: %w", op, err)
			}
			whereConditions := db.Statement.BuildCondition(where, args...)
			c.Where = clause.Where{Exprs: whereConditions}
		}
		db = db.Clauses(c)
	}
	if opts.WithDebug {
		db = db.Debug()
	}
	if opts.WithTable != "" {
		db = db.Table(opts.WithTable)
	}
	if opts.WithBeforeWrite != nil {
		if err := opts.WithBeforeWrite(i); err != nil {
			return fmt.Errorf("%s: error before write: %w", op, err)
		}
	}
	tx := db.Create(i)
	if tx.Error != nil {
		return fmt.Errorf("%s: create failed: %w", op, tx.Error)
	}
	if opts.WithRowsAffected != nil {
		*opts.WithRowsAffected = tx.RowsAffected
	}
	if tx.RowsAffected > 0 && opts.WithAfterWrite != nil {
		if err := opts.WithAfterWrite(i, int(tx.RowsAffected)); err != nil {
			return fmt.Errorf("%s: error after write: %w", op, err)
		}
	}
	if err := rw.lookupAfterWrite(ctx, i, opt...); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// CreateItems will create multiple items of the same type. Supported options:
// WithDebug, WithBeforeWrite, WithAfterWrite, WithReturnRowsAffected,
// OnConflict, WithVersion, WithTable, and WithWhere. WithLookup is not a supported option.
func (rw *RW) CreateItems(ctx context.Context, createItems []interface{}, opt ...Option) error {
	const op = "dbw.CreateItems"
	if rw.underlying == nil {
		return fmt.Errorf("%s: missing underlying db: %w", op, ErrInvalidParameter)
	}
	if len(createItems) == 0 {
		return fmt.Errorf("%s: missing interfaces: %w", op, ErrInvalidParameter)
	}
	if err := raiseErrorOnHooks(createItems); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	opts := GetOpts(opt...)
	if opts.WithLookup {
		return fmt.Errorf("%s: with lookup not a supported option: %w", op, ErrInvalidParameter)
	}
	// verify that createItems are all the same type.
	var foundType reflect.Type
	for i, v := range createItems {
		if i == 0 {
			foundType = reflect.TypeOf(v)
		}
		currentType := reflect.TypeOf(v)
		if foundType != currentType {
			return fmt.Errorf("%s: create items contains disparate types. item %d is not a %s: %w", op, i, foundType.Name(), ErrInvalidParameter)
		}
	}
	if opts.WithBeforeWrite != nil {
		if err := opts.WithBeforeWrite(createItems); err != nil {
			return fmt.Errorf("%s: error before write: %w", op, err)
		}
	}
	var rowsAffected int64
	for _, item := range createItems {
		if err := rw.Create(ctx, item,
			WithOnConflict(opts.WithOnConflict),
			WithReturnRowsAffected(&rowsAffected),
			WithDebug(opts.WithDebug),
			WithVersion(opts.WithVersion),
			WithWhere(opts.WithWhereClause, opts.WithWhereClauseArgs...),
			WithTable(opts.WithTable),
		); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if opts.WithRowsAffected != nil {
		*opts.WithRowsAffected = rowsAffected
	}
	if opts.WithAfterWrite != nil {
		if err := opts.WithAfterWrite(createItems, int(rowsAffected)); err != nil {
			return fmt.Errorf("%s: error after write: %w", op, err)
		}
	}
	return nil
}

func setFieldsToNil(i interface{}, fieldNames []string) {
	// Note: error cases are not handled
	_ = Clear(i, fieldNames, 2)
}

// Clear sets fields in the value pointed to by i to their zero value.
// Clear descends i to depth clearing fields at each level. i must be a
// pointer to a struct. Cycles in i are not detected.
//
// A depth of 2 will change i and i's children. A depth of 1 will change i
// but no children of i. A depth of 0 will return with no changes to i.
func Clear(i interface{}, fields []string, depth int) error {
	const op = "dbw.Clear"
	if len(fields) == 0 || depth == 0 {
		return nil
	}
	fm := make(map[string]bool)
	for _, f := range fields {
		fm[f] = true
	}

	v := reflect.ValueOf(i)

	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() || v.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("%s: %w", op, ErrInvalidParameter)
		}
		clear(v, fm, depth)
	default:
		return fmt.Errorf("%s: %w", op, ErrInvalidParameter)
	}
	return nil
}

func clear(v reflect.Value, fields map[string]bool, depth int) {
	if depth == 0 {
		return
	}
	depth--

	switch v.Kind() {
	case reflect.Ptr:
		clear(v.Elem(), fields, depth+1)
	case reflect.Struct:
		typeOfT := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if ok := fields[typeOfT.Field(i).Name]; ok {
				if f.IsValid() && f.CanSet() {
					f.Set(reflect.Zero(f.Type()))
				}
				continue
			}
			clear(f, fields, depth)
		}
	}
}

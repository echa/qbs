package qbs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

// DEPRECATED: required for testcases; works with the legacy Register API
var driver, driverSource, dbName string
var connectionLimit chan struct{}
var blockingOnLimit bool
var ConnectionLimitError = errors.New("Connection limit reached")
var db *sql.DB
var dial Dialect

// defaults
var queryLogger *log.Logger = log.New(os.Stdout, "qbs:", log.LstdFlags)
var errorLogger *log.Logger = log.New(os.Stderr, "qbs:", log.LstdFlags)
var defaultSchema string

// default instance
var qbs *Qbs
var stmtCache = &sqlStmtCache{
	stmtMap: make(map[string]*sql.Stmt),
}

// prepared statement cache
type sqlStmtCache struct {
	sync.RWMutex
	stmtMap map[string]*sql.Stmt
}

func (c *sqlStmtCache) Get(q string) (stmt *sql.Stmt, ok bool) {
	c.RLock()
	defer c.RUnlock()
	stmt, ok = c.stmtMap[q]
	return
}

func (c *sqlStmtCache) Add(q string, s *sql.Stmt) {
	c.Lock()
	defer c.Unlock()
	c.stmtMap[q] = s
}

func (c *sqlStmtCache) Clear() {
	c.Lock()
	defer c.Unlock()
	for _, v := range c.stmtMap {
		v.Close()
	}
	c.stmtMap = make(map[string]*sql.Stmt)
}

type Qbs struct {
	Dialect Dialect
	Log     bool //Set to true to print out sql statement.

	schema string
	ctx    context.Context
	db     *sql.DB

	tx           *sql.Tx
	txStmtMap    map[string]*sql.Stmt
	stmtCache    *sqlStmtCache
	criteria     *criteria
	firstTxError error
	queryLogger  *log.Logger
	errorLogger  *log.Logger
}

type Validator interface {
	Validate(*Qbs) error
}

// set a schema (supported by PostgreSQL)
func SetDefaultSchema(s string) {
	defaultSchema = strings.TrimSuffix(s, ".") + "."
}

func GetDefaultSchema() string {
	return defaultSchema
}

func SetDefaultLogger(query *log.Logger, err *log.Logger) {
	queryLogger = query
	errorLogger = err
}

func Connect(driver, url string, dialect Dialect) (*Qbs, error) {
	database, err := sql.Open(driver, url)
	if err != nil {
		return nil, err
	}
	return ConnectWithDb(driver, database, dialect)
}

func ConnectWithDb(driver string, database *sql.DB, dialect Dialect) (*Qbs, error) {
	database.SetMaxIdleConns(100)
	return &Qbs{
		Dialect:   dialect,
		schema:    defaultSchema,
		db:        database,
		criteria:  &criteria{},
		stmtCache: stmtCache,
	}, nil
}

//Register a database, should be call at the beginning of the application.
func Register(driverName, driverUrl, databaseName string, dialect Dialect) {
	driver, driverSource, dbName, dial = driverName, driverUrl, databaseName, dialect
	db, err := Connect(driver, driverUrl, dialect)
	if err != nil {
		return
	}
	qbs = db
}

//A safe and easy way to work with *Qbs instance without the need to open and close it.
func WithQbs(task func(*Qbs) error) error {
	db, err := GetQbs()
	if err != nil {
		return err
	}
	defer func() {
		if db.tx != nil {
			db.Rollback()
		}
	}()
	return task(db)
}

//Get an Qbs instance, should call `defer q.Close()` next, like:
//
//		q, err := qbs.GetQbs()
//	  	if err != nil {
//			fmt.Println(err)
//			return
//		}
//		defer q.Close()
//		...
//
func GetQbs() (*Qbs, error) {
	return qbs, nil
}

// DEPRECATED
//Set the connection limit, there is no limit by default.
//If blocking is true, GetQbs method will be blocked, otherwise returns ConnectionLimitError.
func SetConnectionLimit(maxCon int, blocking bool) {
	if maxCon > 0 {
		connectionLimit = make(chan struct{}, maxCon)
	} else if maxCon < 0 {
		connectionLimit = nil
	}
	blockingOnLimit = blocking
}

// Creates a private Qbs instance that uses the specified context.
func (q *Qbs) WithContext(ctx context.Context) *Qbs {
	return &Qbs{
		ctx:         ctx,
		Dialect:     q.Dialect,
		Log:         q.Log,
		schema:      q.schema,
		db:          q.db,
		criteria:    &criteria{},
		tx:          nil,
		stmtCache:   q.stmtCache,
		txStmtMap:   nil,
		queryLogger: q.queryLogger,
		errorLogger: q.errorLogger,
	}
}

func (q *Qbs) SetSchema(s string) {
	q.schema = strings.TrimSuffix(s, ".") + "."
}

func (q *Qbs) GetSchema() string {
	return q.schema
}

func (q *Qbs) GetStats() sql.DBStats {
	return q.db.Stats()
}

//The default connection pool size is 100.
func (q *Qbs) ChangePoolSize(size int) {
	q.db.SetMaxIdleConns(size)
	q.db.SetMaxOpenConns(size)
}

func (q *Qbs) SetLogger(query *log.Logger, err *log.Logger) {
	q.queryLogger = query
	q.errorLogger = err
}

// Create a new criteria for subsequent query
func (q *Qbs) Reset() {
	q.criteria = new(criteria)
}

// Begin create a transaction object internally
// You can perform queries with the same Qbs object
// no matter it is in transaction or not.
func (q *Qbs) Begin() error {
	return q.BeginTx(q.ctx, nil)
}

func (q *Qbs) BeginTx(ctx context.Context, opts *sql.TxOptions) error {
	if q.tx != nil {
		return fmt.Errorf("cannot start nested transaction")
	}
	tx, err := q.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	q.ctx = ctx
	q.tx = tx
	q.txStmtMap = make(map[string]*sql.Stmt)
	return nil
}

func (q *Qbs) InTransaction() bool {
	return q.tx != nil
}

func (q *Qbs) updateTxError(e error) error {
	if e != nil {
		if q.errorLogger != nil {
			q.errorLogger.Println("qbs:", e)
		}
		// don't shadow the first error
		if q.firstTxError == nil {
			q.firstTxError = e
		}
	}
	return e
}

// Commit commits a started transaction and will report the first error that
// occurred inside the transaction.
func (q *Qbs) Commit() error {
	err := q.tx.Commit()
	if err != nil {
		q.errorLogger.Println("qbs: commit error:", err)
	}
	q.updateTxError(err)
	q.tx = nil
	for _, v := range q.txStmtMap {
		v.Close()
	}
	q.txStmtMap = nil
	return q.firstTxError
}

// Rollback rolls back a started transaction.
func (q *Qbs) Rollback() error {
	err := q.tx.Rollback()
	if err != nil {
		q.errorLogger.Println("qbs: rollback error:", err)
	}
	q.tx = nil
	for _, v := range q.txStmtMap {
		v.Close()
	}
	q.txStmtMap = nil
	return q.updateTxError(err)
}

// Where is a shortcut method to call Condtion(NewCondtition(expr, args...)).
func (q *Qbs) Where(expr string, args ...interface{}) *Qbs {
	q.criteria.condition = NewCondition(expr, args...)
	return q
}

//Snakecase column name
func (q *Qbs) WhereEqual(column string, value interface{}) *Qbs {
	q.criteria.condition = NewEqualCondition(column, value)
	return q
}

func (q *Qbs) WhereIn(column string, values []interface{}) *Qbs {
	q.criteria.condition = NewInCondition(column, values)
	return q
}

//Condition defines the SQL "WHERE" clause
//If other condition can be inferred by the struct argument in
//Find method, it will be merged with AND
func (q *Qbs) Condition(condition *Condition) *Qbs {
	q.criteria.condition = condition
	return q
}

func (q *Qbs) Limit(limit int) *Qbs {
	q.criteria.limit = limit
	return q
}

func (q *Qbs) Offset(offset int) *Qbs {
	q.criteria.offset = offset
	return q
}

func (q *Qbs) Order(path string, desc bool) *Qbs {
	q.criteria.orderBys = append(q.criteria.orderBys, order{q.Dialect.quote(path), desc})
	return q
}

func (q *Qbs) OrderBy(path string) *Qbs {
	q.criteria.orderBys = append(q.criteria.orderBys, order{q.Dialect.quote(path), false})
	return q
}

func (q *Qbs) OrderByDesc(path string) *Qbs {
	q.criteria.orderBys = append(q.criteria.orderBys, order{q.Dialect.quote(path), true})
	return q
}

// Camel case field names
func (q *Qbs) OmitFields(fieldName ...string) *Qbs {
	q.criteria.omitFields = fieldName
	return q
}

func (q *Qbs) OmitJoin() *Qbs {
	q.criteria.omitJoin = true
	return q
}

// Perform select query by parsing the struct's type and then fill the values into the struct
// All fields of supported types in the struct will be added in select clause.
// If Id value is provided, it will be added into the where clause
// If a foreign key field with its referenced struct pointer field are provided,
// It will perform a join query, the referenced struct pointer field will be filled in
// the values obtained by the query.
// If not found, "sql.ErrNoRows" will be returned.
func (q *Qbs) Find(structPtr interface{}) error {
	q.criteria.model = structPtrToModel(structPtr, !q.criteria.omitJoin, q.criteria.omitFields, q.schema)
	q.criteria.limit = 1
	if !q.criteria.model.pkZero() {
		idPath := q.Dialect.quote(q.criteria.model.table) + "." + q.Dialect.quote(q.criteria.model.pk.name)
		idCondition := NewCondition(idPath+" = ?", q.criteria.model.pk.value)
		if q.criteria.condition == nil {
			q.criteria.condition = idCondition
		} else {
			q.criteria.condition = idCondition.AndCondition(q.criteria.condition)
		}
	}
	query, args := q.Dialect.querySql(q.criteria)
	return q.doQueryRow(structPtr, query, args...)
}

// Similar to Find, except that FindAll accept pointer of slice of struct pointer,
// rows will be appended to the slice.
func (q *Qbs) FindAll(ptrOfSliceOfStructPtr interface{}) error {
	strucType := reflect.TypeOf(ptrOfSliceOfStructPtr).Elem().Elem().Elem()
	strucPtr := reflect.New(strucType).Interface()
	q.criteria.model = structPtrToModel(strucPtr, !q.criteria.omitJoin, q.criteria.omitFields, q.schema)
	query, args := q.Dialect.querySql(q.criteria)
	return q.doQueryRows(ptrOfSliceOfStructPtr, query, args...)
}

func (q *Qbs) doQueryRow(out interface{}, query string, args ...interface{}) error {
	defer q.Reset()
	rowValue := reflect.ValueOf(out)
	q.log(query, args...)
	stmt, err := q.prepare(query)
	if err != nil {
		return q.updateTxError(err)
	}
	rows, err := stmt.QueryContext(q.ctx, args...)
	if err != nil {
		return q.updateTxError(err)
	}
	defer rows.Close()
	if rows.Next() {
		err = q.scanRows(rowValue, rows)
		if err != nil {
			return err
		}
	} else {
		return sql.ErrNoRows
	}
	return nil
}

func (q *Qbs) doQueryRows(out interface{}, query string, args ...interface{}) error {
	defer q.Reset()
	sliceValue := reflect.Indirect(reflect.ValueOf(out))
	structType := sliceValue.Type().Elem().Elem()
	q.log(query, args...)
	stmt, err := q.prepare(query)
	if err != nil {
		return q.updateTxError(err)
	}
	rows, err := stmt.QueryContext(q.ctx, args...)
	if err != nil {
		return q.updateTxError(err)
	}
	defer rows.Close()
	for rows.Next() {
		rowValue := reflect.New(structType)
		err = q.scanRows(rowValue, rows)
		if err != nil {
			return err
		}
		sliceValue.Set(reflect.Append(sliceValue, rowValue))
	}
	return nil
}

func (q *Qbs) scanRows(rowValue reflect.Value, rows *sql.Rows) error {
	cols, err := rows.Columns()
	if err != nil {
		return q.updateTxError(err)
	}
	containers := make([]interface{}, 0, len(cols))
	for i := 0; i < cap(containers); i++ {
		var v interface{}
		containers = append(containers, &v)
	}
	err = rows.Scan(containers...)
	if err != nil {
		return err
	}
	for i, v := range containers {
		value := reflect.Indirect(reflect.ValueOf(v))
		if !value.Elem().IsValid() {
			continue
		}
		key := cols[i]
		paths := strings.Split(key, "___")
		if len(paths) == 2 {
			subStruct := rowValue.Elem().FieldByName(TableNameToStructName(paths[0]))
			if subStruct.IsNil() {
				subStruct.Set(reflect.New(subStruct.Type().Elem()))
			}
			subField := subStruct.Elem().FieldByName(ColumnNameToFieldName(paths[1]))
			if subField.IsValid() {
				err = q.Dialect.setModelValue(value, subField)
				if err != nil {
					return err
				}
			}
		} else {
			field := rowValue.Elem().FieldByName(ColumnNameToFieldName(key))
			if field.IsValid() {
				err = q.Dialect.setModelValue(value, field)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Same as sql.Db.Exec or sql.Tx.Exec depends on if transaction has began
func (q *Qbs) Exec(query string, args ...interface{}) (sql.Result, error) {
	defer q.Reset()
	query = q.Dialect.substituteMarkers(query)
	q.log(query, args...)
	stmt, err := q.prepare(query)
	if err != nil {
		return nil, q.updateTxError(err)
	}
	result, err := stmt.ExecContext(q.ctx, args...)
	if err != nil {
		return nil, q.updateTxError(err)
	}
	return result, nil
}

// Same as sql.Db.QueryRow or sql.Tx.QueryRow depends on if transaction has began
func (q *Qbs) QueryRow(query string, args ...interface{}) (*sql.Row, error) {
	q.log(query, args...)
	query = q.Dialect.substituteMarkers(query)
	stmt, err := q.prepare(query)
	if err != nil {
		q.updateTxError(err)
		return nil, err
	}
	return stmt.QueryRowContext(q.ctx, args...), nil
}

// Same as sql.Db.Query or sql.Tx.Query depends on if transaction has began
func (q *Qbs) Query(query string, args ...interface{}) (*sql.Rows, error) {
	q.log(query, args...)
	query = q.Dialect.substituteMarkers(query)
	stmt, err := q.prepare(query)
	if err != nil {
		return nil, q.updateTxError(err)
	}
	return stmt.QueryContext(q.ctx, args...)
}

// Same as sql.Db.Prepare or sql.Tx.Prepare depends on if transaction has began
func (q *Qbs) prepare(query string) (stmt *sql.Stmt, err error) {
	var ok bool
	if q.tx != nil {
		stmt, ok = q.txStmtMap[query]
		if ok {
			return
		}
		stmt, err = q.tx.PrepareContext(q.ctx, query)
		if err != nil {
			q.updateTxError(err)
			return
		}
		q.txStmtMap[query] = stmt
	} else {
		stmt, ok = q.stmtCache.Get(query)
		if ok {
			return
		}

		stmt, err = q.db.PrepareContext(q.ctx, query+";")
		if err != nil {
			q.updateTxError(err)
			return
		}
		q.stmtCache.Add(query, stmt)
	}
	return
}

// If Id value is not provided, save will insert the record, and the Id value will
// be filled in the struct after insertion.
// If Id value is provided, save will do a query count first to see if the row exists, if not then insert it,
// otherwise update it.
// If struct implements Validator interface, it will be validated first
func (q *Qbs) Save(structPtr interface{}) (affected int64, err error) {
	if v, ok := structPtr.(Validator); ok {
		err = v.Validate(q)
		if err != nil {
			return
		}
	}
	model := structPtrToModel(structPtr, true, q.criteria.omitFields, q.schema)
	if model.pk == nil {
		return 0, fmt.Errorf("missing primary key field")
	}
	q.criteria.model = model
	now := time.Now()
	var id int64 = 0
	updateModelField := model.timeField("updated")
	if updateModelField != nil {
		updateModelField.value = now
	}
	createdModelField := model.timeField("created")
	var isInsert bool
	cnt, err := q.WhereEqual(model.pk.name, model.pk.value).Count(model.table)
	if !model.pkZero() && cnt > 0 { //id is given, can be an update operation.
		affected, err = q.Dialect.update(q)
	} else {
		if createdModelField != nil {
			createdModelField.value = now
		}
		id, err = q.Dialect.insert(q)
		isInsert = true
		if err == nil {
			affected = 1
		}
	}
	if err == nil {
		structValue := reflect.Indirect(reflect.ValueOf(structPtr))
		if _, ok := model.pk.value.(IdType); ok && id != 0 {
			idField := structValue.FieldByName(model.pk.camelName)
			idField.SetInt(id)
		} else if _, ok := model.pk.value.(int64); ok && id != 0 {
			idField := structValue.FieldByName(model.pk.camelName)
			idField.SetInt(id)
		} else if _, ok := model.pk.value.(uint64); ok && id != 0 {
			idField := structValue.FieldByName(model.pk.camelName)
			idField.SetUint((uint64)(id))
		}
		if updateModelField != nil {
			updateField := structValue.FieldByName(updateModelField.camelName)
			updateField.Set(reflect.ValueOf(now))
		}
		if isInsert {
			if createdModelField != nil {
				createdField := structValue.FieldByName(createdModelField.camelName)
				createdField.Set(reflect.ValueOf(now))
			}
		}
	}
	return affected, q.updateTxError(err)
}

func (q *Qbs) BulkInsert(sliceOfStructPtr interface{}) error {
	defer q.Reset()
	var err error
	if q.tx == nil {
		q.Begin()
		defer func() {
			if err != nil {
				q.Rollback()
			} else {
				q.Commit()
			}
		}()
	}
	sliceValue := reflect.ValueOf(sliceOfStructPtr)
	for i := 0; i < sliceValue.Len(); i++ {
		structPtr := sliceValue.Index(i)
		structPtrInter := structPtr.Interface()
		if v, ok := structPtrInter.(Validator); ok {
			err = v.Validate(q)
			if err != nil {
				return q.updateTxError(err)
			}
		}
		model := structPtrToModel(structPtrInter, false, nil, q.schema)
		if model.pk == nil {
			return fmt.Errorf("missing primary key field")
		}
		q.criteria.model = model
		var id int64
		id, err = q.Dialect.insert(q)
		if err != nil {
			return q.updateTxError(err)
		}
		if _, ok := model.pk.value.(int64); ok && id != 0 {
			idField := structPtr.Elem().FieldByName(model.pk.camelName)
			idField.SetInt(id)
		}
	}
	return nil
}

// If the struct type implements Validator interface, values will be validated before update.
// In order to avoid inadvertently update the struct field to zero value, it is better to define a
// temporary struct in function, only define the fields that should be updated.
// But the temporary struct can not implement Validator interface, we have to validate values manually.
// The update condition can be inferred by the Id value of the struct.
func (q *Qbs) Update(structPtr interface{}) (affected int64, err error) {
	if v, ok := structPtr.(Validator); ok {
		err := v.Validate(q)
		if err != nil {
			return 0, err
		}
	}
	model := structPtrToModel(structPtr, true, q.criteria.omitFields, q.schema)
	q.criteria.model = model
	q.criteria.mergePkCondition(q.Dialect)
	if q.criteria.condition == nil {
		return 0, fmt.Errorf("missing condition for update")
	}
	return q.Dialect.update(q)
}

// The delete condition can be inferred by the Id value of the struct
func (q *Qbs) Delete(structPtr interface{}) (affected int64, err error) {
	model := structPtrToModel(structPtr, true, q.criteria.omitFields, q.schema)
	q.criteria.model = model
	q.criteria.mergePkCondition(q.Dialect)
	if q.criteria.condition == nil {
		return 0, fmt.Errorf("missing condition for delete")
	}
	return q.Dialect.delete(q)
}

// This method can be used to validate unique column before trying to save
// The table parameter can be either a string or a struct pointer
func (q *Qbs) ContainsValue(table interface{}, column string, value interface{}) bool {
	quotedColumn := q.Dialect.quote(column)
	quotedTable := q.Dialect.quote(tableName(table))
	query := fmt.Sprintf("SELECT %v FROM %v WHERE %v = ?", quotedColumn, quotedTable, quotedColumn)
	row, err := q.QueryRow(query, value)
	if err != nil {
		q.updateTxError(err)
		return false
	}
	var result interface{}
	err = row.Scan(&result)
	q.updateTxError(err)
	return err == nil
}

// If the connection pool is not full, the Db will be sent back into the pool, otherwise the Db will get closed.
func (q *Qbs) Close() error {
	// if connectionLimit != nil {
	// 	<-connectionLimit
	// }
	if q.tx != nil {
		return q.Rollback()
	}
	q.tx = nil
	q.firstTxError = nil
	return nil
}

func (q *Qbs) CloseDB() error {
	if err := q.Close(); err != nil {
		return err
	}
	q.stmtCache.Clear()
	return q.db.Close()
}

//Query the count of rows in a table the table parameter can be either a string or struct pointer.
//If condition is given, the count will be the count of rows meet that condition.
func (q *Qbs) Count(table interface{}) (int64, error) {
	quotedTable := q.Dialect.quote(tableName(table))
	// add prefix if it does not already exist
	if !strings.HasPrefix(quotedTable, q.schema) {
		quotedTable = q.schema + quotedTable
	}
	query := "SELECT COUNT(*) FROM " + quotedTable
	var row *sql.Row
	var err error
	if q.criteria.condition != nil {
		conditionSql, args := q.criteria.condition.Merge()
		query += " WHERE " + conditionSql
		row, err = q.QueryRow(query, args...)
	} else {
		row, err = q.QueryRow(query)
	}
	if err != nil {
		return 0, err
	}
	var count int64
	err = row.Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	} else if err != nil {
		q.updateTxError(err)
		return 0, err
	}
	return count, nil
}

func (q *Qbs) CountJoin(ptrOfSliceOfStructPtr interface{}) (int64, error) {
	defer q.Reset()
	strucType := reflect.TypeOf(ptrOfSliceOfStructPtr).Elem().Elem().Elem()
	strucPtr := reflect.New(strucType).Interface()
	q.criteria.model = structPtrToModel(strucPtr, !q.criteria.omitJoin, q.criteria.omitFields, q.schema)
	table := q.Dialect.quote(q.criteria.model.table)
	tables := []string{table}
	// hasJoin := len(q.criteria.model.refs) > 0
	for k, v := range q.criteria.model.refs {
		tableAlias := StructNameToTableName(k)
		quotedTableAlias := q.Dialect.quote(tableAlias)
		quotedParentTable := q.Dialect.quote(v.model.table)
		leftKey := table + "." + q.Dialect.quote(v.refKey)
		parentPrimary := quotedTableAlias + "." + q.Dialect.quote(v.model.pk.name)
		joinClause := fmt.Sprintf("LEFT JOIN %v AS %v ON %v = %v", quotedParentTable, quotedTableAlias, leftKey, parentPrimary)
		tables = append(tables, joinClause)
	}
	query := "SELECT COUNT(*) FROM " + strings.Join(tables, " ")
	var row *sql.Row
	var err error
	if q.criteria.condition != nil {
		conditionSql, args := q.criteria.condition.Merge()
		query += " WHERE " + conditionSql
		row, err = q.QueryRow(query, args...)
	} else {
		row, err = q.QueryRow(query)
	}
	if err != nil {
		return 0, err
	}
	var count int64
	err = row.Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	} else if err != nil {
		q.updateTxError(err)
		return 0, err
	}
	return count, nil
}

//Query raw sql and return a map.
func (q *Qbs) QueryMap(query string, args ...interface{}) (map[string]interface{}, error) {
	mapSlice, err := q.doQueryMap(query, true, args...)
	if len(mapSlice) == 1 {
		return mapSlice[0], err
	}
	return nil, sql.ErrNoRows

}

//Query raw sql and return a slice of map..
func (q *Qbs) QueryMapSlice(query string, args ...interface{}) ([]map[string]interface{}, error) {
	return q.doQueryMap(query, false, args...)
}

func (q *Qbs) doQueryMap(query string, once bool, args ...interface{}) ([]map[string]interface{}, error) {
	query = q.Dialect.substituteMarkers(query)
	stmt, err := q.prepare(query)
	if err != nil {
		return nil, q.updateTxError(err)
	}
	rows, err := stmt.QueryContext(q.ctx, args...)
	if err != nil {
		return nil, q.updateTxError(err)
	}
	defer rows.Close()
	var results []map[string]interface{}
	columns, err := rows.Columns()
	if err != nil {
		return nil, q.updateTxError(err)
	}
	containers := make([]interface{}, len(columns))
	for i := 0; i < len(columns); i++ {
		var container interface{}
		containers[i] = &container
	}
	for rows.Next() {
		if err := rows.Scan(containers...); err != nil {
			return nil, q.updateTxError(err)
		}
		result := make(map[string]interface{}, len(columns))
		for i, key := range columns {
			if containers[i] == nil {
				continue
			}
			value := reflect.Indirect(reflect.ValueOf(containers[i]))
			if value.Elem().Kind() == reflect.Slice {
				result[key] = string(value.Interface().([]byte))
			} else {
				result[key] = value.Interface()
			}
		}
		results = append(results, result)
		if once {
			return results, nil
		}
	}
	return results, nil
}

//Do a raw sql query and set the result values in dest parameter.
//The dest parameter can be either a struct pointer or a pointer of struct pointer.slice
//This method do not support pointer field in the struct.
func (q *Qbs) QueryStruct(dest interface{}, query string, args ...interface{}) error {
	query = q.Dialect.substituteMarkers(query)
	stmt, err := q.prepare(query)
	if err != nil {
		return q.updateTxError(err)
	}
	rows, err := stmt.QueryContext(q.ctx, args...)
	if err != nil {
		return q.updateTxError(err)
	}
	defer rows.Close()
	outPtr := reflect.ValueOf(dest)
	outValue := outPtr.Elem()
	var structType reflect.Type
	var single bool
	if outValue.Kind() == reflect.Slice {
		structType = outValue.Type().Elem().Elem()
	} else {
		structType = outValue.Type()
		single = true
	}
	columns, err := rows.Columns()
	if err != nil {
		return q.updateTxError(err)
	}
	fieldNames := make([]string, len(columns))
	for i, v := range columns {
		upper := snakeToUpperCamel(v)
		_, ok := structType.FieldByName(upper)
		if ok {
			fieldNames[i] = upper
		} else {
			fieldNames[i] = "-"
		}
	}
	for rows.Next() {
		var rowStructPointer reflect.Value
		if single { //query row
			rowStructPointer = outPtr
		} else { //query rows
			rowStructPointer = reflect.New(structType)
		}
		dests := make([]interface{}, len(columns))
		for i := 0; i < len(dests); i++ {
			fieldName := fieldNames[i]
			if fieldName == "-" {
				var placeholder interface{}
				dests[i] = &placeholder
			} else {
				field := rowStructPointer.Elem().FieldByName(fieldName)
				dests[i] = field.Addr().Interface()
			}
		}
		err = rows.Scan(dests...)
		if err != nil {
			return err
		}
		if single {
			return nil
		}
		outValue.Set(reflect.Append(outValue, rowStructPointer))
	}
	return nil
}

//Iterate the rows, the first parameter is a struct pointer, the second parameter is a fucntion
//which will get called on each row, the in `do` function the structPtr's value will be set to the current row's value..
//if `do` function returns an error, the iteration will be stopped.
func (q *Qbs) Iterate(structPtr interface{}, do func() error) error {
	q.criteria.model = structPtrToModel(structPtr, !q.criteria.omitJoin, q.criteria.omitFields, q.schema)
	query, args := q.Dialect.querySql(q.criteria)
	q.log(query, args...)
	defer q.Reset()
	stmt, err := q.prepare(query)
	if err != nil {
		return q.updateTxError(err)
	}
	rows, err := stmt.QueryContext(q.ctx, args...)
	if err != nil {
		return q.updateTxError(err)
	}
	rowValue := reflect.ValueOf(structPtr)
	defer rows.Close()
	for rows.Next() {
		err = q.scanRows(rowValue, rows)
		if err != nil {
			return err
		}
		if err = do(); err != nil {
			return err
		}
	}
	return nil
}

func (q *Qbs) log(query string, args ...interface{}) {
	if q.Log && q.queryLogger != nil {
		q.queryLogger.Print("qbs:", query)
		q.queryLogger.Println(args...)
	}
}

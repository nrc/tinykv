package tikv

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/codec"
	tipb "github.com/pingcap/tipb/go-tipb"
	"golang.org/x/net/context"
)

var (
	_ executor = &tableScanExec{}
	_ executor = &indexScanExec{}
	_ executor = &selectionExec{}
	_ executor = &limitExec{}
	_ executor = &topNExec{}
)

type executor interface {
	SetSrcExec(executor)
	GetSrcExec() executor
	ResetCounts()
	Counts() []int64
	Next(ctx context.Context) ([][]byte, error)
	// Cursor returns the key gonna to be scanned by the Next() function.
	Cursor() (key []byte, desc bool)
}

type tableScanExec struct {
	*tipb.TableScan
	colIDs         map[int64]int
	kvRanges       []kv.KeyRange
	startTS        uint64
	isolationLevel kvrpcpb.IsolationLevel
	mvccStore      *MVCCStore
	regCtx         *regionCtx
	rangeCursor    int

	rowCursor int
	rows      [][][]byte
	seekKey   []byte
	start     int
	counts    []int64

	src executor
}

func (e *tableScanExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *tableScanExec) GetSrcExec() executor {
	return e.src
}

func (e *tableScanExec) ResetCounts() {
	if e.counts != nil {
		e.start = e.rangeCursor
		e.counts[e.start] = 0
	}
}

func (e *tableScanExec) Counts() []int64 {
	if e.counts == nil {
		return nil
	}
	if e.seekKey == nil {
		return e.counts[e.start:e.rangeCursor]
	}
	return e.counts[e.start : e.rangeCursor+1]
}

func (e *tableScanExec) Cursor() ([]byte, bool) {
	if len(e.seekKey) > 0 {
		return e.seekKey, e.Desc
	}

	if e.rangeCursor < len(e.kvRanges) {
		ran := e.kvRanges[e.rangeCursor]
		if ran.IsPoint() {
			return ran.StartKey, e.Desc
		}

		if e.Desc {
			return ran.EndKey, e.Desc
		}
		return ran.StartKey, e.Desc
	}

	if e.Desc {
		return e.kvRanges[len(e.kvRanges)-1].StartKey, e.Desc
	}
	return e.kvRanges[len(e.kvRanges)-1].EndKey, e.Desc
}

func (e *tableScanExec) Next(ctx context.Context) (value [][]byte, err error) {
	for {
		if e.rowCursor < len(e.rows) {
			value = e.rows[e.rowCursor]
			e.rowCursor++
			return value, nil
		}
		e.rowCursor = 0
		e.rows = e.rows[:0]
		err := e.fillRows()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if len(e.rows) == 0 {
			break
		}
	}
	return nil, nil
}

func (e *tableScanExec) fillRows() error {
	for e.rangeCursor < len(e.kvRanges) {
		ran := e.kvRanges[e.rangeCursor]
		var err error
		if ran.IsPoint() {
			err = e.fillRowsFromPoint(ran)
			e.rangeCursor++
			e.seekKey = nil
		} else {
			err = e.fillRowsFromRange(ran)
			if len(e.rows) == 0 {
				e.rangeCursor++
				e.seekKey = nil
			}
		}
		if err != nil {
			return errors.Trace(err)
		}
		if len(e.rows) > 0 {
			return nil
		}
	}
	return nil
}

func (e *tableScanExec) fillRowsFromPoint(ran kv.KeyRange) error {
	val, err := e.mvccStore.Get(e.regCtx, ran.StartKey, e.startTS)
	if err != nil {
		return errors.Trace(err)
	}
	if len(val) == 0 {
		return nil
	}
	handle, err := tablecodec.DecodeRowKey(ran.StartKey)
	if err != nil {
		return errors.Trace(err)
	}
	row, err := getRowData(e.Columns, e.colIDs, handle, val)
	if err != nil {
		return errors.Trace(err)
	}
	e.rows = append(e.rows, row)
	return nil
}

const scanLimit = 128

func (e *tableScanExec) fillRowsFromRange(ran kv.KeyRange) error {
	if e.seekKey == nil {
		if e.Desc {
			e.seekKey = ran.EndKey
		} else {
			e.seekKey = ran.StartKey
		}
	}
	var pairs []Pair
	if e.Desc {
		pairs = e.mvccStore.ReverseScan(e.regCtx, ran.StartKey, e.seekKey, scanLimit, e.startTS)
	} else {
		pairs = e.mvccStore.Scan(e.regCtx, e.seekKey, ran.EndKey, scanLimit, e.startTS)
	}
	if len(pairs) == 0 {
		return nil
	}
	for _, pair := range pairs {
		if pair.Err != nil {
			return errors.Trace(pair.Err)
		}
		handle, err := tablecodec.DecodeRowKey(pair.Key)
		if err != nil {
			return errors.Trace(err)
		}
		row, err := getRowData(e.Columns, e.colIDs, handle, pair.Value)
		if err != nil {
			return errors.Trace(err)
		}
		e.rows = append(e.rows, row)
	}
	lastPair := pairs[len(pairs)-1]
	if e.Desc {
		e.seekKey = prefixPrev(lastPair.Key)
	} else {
		e.seekKey = []byte(kv.Key(lastPair.Key).PrefixNext())
	}
	return nil
}

const (
	pkColNotExists = iota
	pkColIsSigned
	pkColIsUnsigned
)

type indexScanExec struct {
	*tipb.IndexScan
	colsLen        int
	kvRanges       []kv.KeyRange
	startTS        uint64
	isolationLevel kvrpcpb.IsolationLevel
	mvccStore      *MVCCStore
	regCtx         *regionCtx
	ranCursor      int
	seekKey        []byte
	pkStatus       int
	start          int
	counts         []int64

	rowCursor int
	rows      [][][]byte
	src       executor
}

func (e *indexScanExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *indexScanExec) GetSrcExec() executor {
	return e.src
}

func (e *indexScanExec) ResetCounts() {
	if e.counts != nil {
		e.start = e.ranCursor
		e.counts[e.start] = 0
	}
}

func (e *indexScanExec) Counts() []int64 {
	if e.counts == nil {
		return nil
	}
	if e.seekKey == nil {
		return e.counts[e.start:e.ranCursor]
	}
	return e.counts[e.start : e.ranCursor+1]
}

func (e *indexScanExec) isUnique() bool {
	return e.Unique != nil && *e.Unique
}

func (e *indexScanExec) Cursor() ([]byte, bool) {
	if len(e.seekKey) > 0 {
		return e.seekKey, e.Desc
	}
	if e.ranCursor < len(e.kvRanges) {
		ran := e.kvRanges[e.ranCursor]
		if e.isUnique() && ran.IsPoint() {
			return ran.StartKey, e.Desc
		}
		if e.Desc {
			return ran.EndKey, e.Desc
		}
		return ran.StartKey, e.Desc
	}
	if e.Desc {
		return e.kvRanges[len(e.kvRanges)-1].StartKey, e.Desc
	}
	return e.kvRanges[len(e.kvRanges)-1].EndKey, e.Desc
}

func (e *indexScanExec) Next(ctx context.Context) (value [][]byte, err error) {
	for {
		if e.rowCursor < len(e.rows) {
			value = e.rows[e.rowCursor]
			e.rowCursor++
			return value, nil
		}
		e.rowCursor = 0
		e.rows = e.rows[:0]
		err = e.fillRows()
		if err != nil {
			return nil, errors.Trace(err)
		}
		if len(e.rows) == 0 {
			break
		}
	}
	return nil, nil
}

func (e *indexScanExec) fillRows() error {
	for e.ranCursor < len(e.kvRanges) {
		ran := e.kvRanges[e.ranCursor]
		var err error
		if e.isUnique() && ran.IsPoint() {
			err = e.fillRowsFromPoint(ran)
			e.ranCursor++
			e.seekKey = nil
		} else {
			err = e.fillRowsFromRange(ran)
			if len(e.rows) == 0 {
				e.ranCursor++
				e.seekKey = nil
			}
		}
		if err != nil {
			return errors.Trace(err)
		}
		if len(e.rows) > 0 {
			break
		}
	}
	return nil
}

// fillRowsFromPoint is only used for unique key.
func (e *indexScanExec) fillRowsFromPoint(ran kv.KeyRange) error {
	val, err := e.mvccStore.Get(e.regCtx, ran.StartKey, e.startTS)
	if err != nil {
		return errors.Trace(err)
	}
	if len(val) == 0 {
		return nil
	}
	row, err := e.decodeIndexKV(Pair{Key: ran.StartKey, Value: val})
	if err != nil {
		return errors.Trace(err)
	}
	e.rows = append(e.rows, row)
	return nil
}

func (e *indexScanExec) decodeIndexKV(pair Pair) ([][]byte, error) {
	values, b, err := tablecodec.CutIndexKeyNew(pair.Key, e.colsLen)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if len(b) > 0 {
		if e.pkStatus != pkColNotExists {
			values = append(values, b)
		}
	} else if e.pkStatus != pkColNotExists {
		handle, err := decodeHandle(pair.Value)
		if err != nil {
			return nil, errors.Trace(err)
		}
		var handleDatum types.Datum
		if e.pkStatus == pkColIsUnsigned {
			handleDatum = types.NewUintDatum(uint64(handle))
		} else {
			handleDatum = types.NewIntDatum(handle)
		}
		handleBytes, err := codec.EncodeValue(nil, b, handleDatum)
		if err != nil {
			return nil, errors.Trace(err)
		}
		values = append(values, handleBytes)
	}
	return values, nil
}

func (e *indexScanExec) fillRowsFromRange(ran kv.KeyRange) error {
	if e.seekKey == nil {
		if e.Desc {
			e.seekKey = ran.EndKey
		} else {
			e.seekKey = ran.StartKey
		}
	}
	var pairs []Pair
	if e.Desc {
		pairs = e.mvccStore.ReverseScan(e.regCtx, ran.StartKey, e.seekKey, scanLimit, e.startTS)
	} else {
		pairs = e.mvccStore.Scan(e.regCtx, e.seekKey, ran.EndKey, scanLimit, e.startTS)
	}
	if len(pairs) == 0 {
		return nil
	}
	for _, pair := range pairs {
		if pair.Err != nil {
			// TODO: Handle lock error.
			return errors.Trace(pair.Err)
		}
		row, err := e.decodeIndexKV(pair)
		if err != nil {
			return errors.Trace(err)
		}
		e.rows = append(e.rows, row)
	}
	lastPair := pairs[len(pairs)-1]
	if e.Desc {
		e.seekKey = prefixPrev(lastPair.Key)
	} else {
		e.seekKey = []byte(kv.Key(lastPair.Key).PrefixNext())
	}
	return nil
}

// previous version of kv.PrefixNext.
func prefixPrev(k []byte) []byte {
	buf := make([]byte, len([]byte(k)))
	copy(buf, []byte(k))
	var i int
	for i = len(k) - 1; i >= 0; i-- {
		buf[i]--
		if buf[i] != 255 {
			break
		}
	}
	if i == -1 {
		return nil
	}
	return buf
}

type selectionExec struct {
	conditions        []expression.Expression
	relatedColOffsets []int
	row               []types.Datum
	evalCtx           *evalContext
	src               executor
}

func (e *selectionExec) SetSrcExec(exec executor) {
	e.src = exec
}

func (e *selectionExec) GetSrcExec() executor {
	return e.src
}

func (e *selectionExec) ResetCounts() {
	e.src.ResetCounts()
}

func (e *selectionExec) Counts() []int64 {
	return e.src.Counts()
}

// evalBool evaluates expression to a boolean value.
func evalBool(exprs []expression.Expression, row types.DatumRow, ctx *stmtctx.StatementContext) (bool, error) {
	for _, expr := range exprs {
		data, err := expr.Eval(row)
		if err != nil {
			return false, errors.Trace(err)
		}
		if data.IsNull() {
			return false, nil
		}

		isBool, err := data.ToBool(ctx)
		if err != nil {
			return false, errors.Trace(err)
		}
		if isBool == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (e *selectionExec) Cursor() ([]byte, bool) {
	return e.src.Cursor()
}

func (e *selectionExec) Next(ctx context.Context) (value [][]byte, err error) {
	for {
		value, err = e.src.Next(ctx)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if value == nil {
			return nil, nil
		}

		err = e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
		if err != nil {
			return nil, errors.Trace(err)
		}
		match, err := evalBool(e.conditions, e.row, e.evalCtx.sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if match {
			return value, nil
		}
	}
}

type topNExec struct {
	heap              *topNHeap
	evalCtx           *evalContext
	relatedColOffsets []int
	orderByExprs      []expression.Expression
	row               types.DatumRow
	cursor            int
	executed          bool

	src executor
}

func (e *topNExec) SetSrcExec(src executor) {
	e.src = src
}

func (e *topNExec) GetSrcExec() executor {
	return e.src
}

func (e *topNExec) ResetCounts() {
	e.src.ResetCounts()
}

func (e *topNExec) Counts() []int64 {
	return e.src.Counts()
}

func (e *topNExec) innerNext(ctx context.Context) (bool, error) {
	value, err := e.src.Next(ctx)
	if err != nil {
		return false, errors.Trace(err)
	}
	if value == nil {
		return false, nil
	}
	err = e.evalTopN(value)
	if err != nil {
		return false, errors.Trace(err)
	}
	return true, nil
}

func (e *topNExec) Cursor() ([]byte, bool) {
	panic("don't not use coprocessor streaming API for topN!")
}

func (e *topNExec) Next(ctx context.Context) (value [][]byte, err error) {
	if !e.executed {
		for {
			hasMore, err := e.innerNext(ctx)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if !hasMore {
				break
			}
		}
		e.executed = true
	}
	if e.cursor >= len(e.heap.rows) {
		return nil, nil
	}
	sort.Sort(&e.heap.topNSorter)
	row := e.heap.rows[e.cursor]
	e.cursor++

	return row.data, nil
}

// evalTopN evaluates the top n elements from the data. The input receives a record including its handle and data.
// And this function will check if this record can replace one of the old records.
func (e *topNExec) evalTopN(value [][]byte) error {
	newRow := &sortRow{
		key: make([]types.Datum, len(value)),
	}
	err := e.evalCtx.decodeRelatedColumnVals(e.relatedColOffsets, value, e.row)
	if err != nil {
		return errors.Trace(err)
	}
	for i, expr := range e.orderByExprs {
		newRow.key[i], err = expr.Eval(e.row)
		if err != nil {
			return errors.Trace(err)
		}
	}

	if e.heap.tryToAddRow(newRow) {
		for _, val := range value {
			newRow.data = append(newRow.data, val)
		}
	}
	return errors.Trace(e.heap.err)
}

type limitExec struct {
	limit  uint64
	cursor uint64

	src executor
}

func (e *limitExec) SetSrcExec(src executor) {
	e.src = src
}

func (e *limitExec) GetSrcExec() executor {
	return e.src
}

func (e *limitExec) ResetCounts() {
	e.src.ResetCounts()
}

func (e *limitExec) Counts() []int64 {
	return e.src.Counts()
}

func (e *limitExec) Cursor() ([]byte, bool) {
	return e.src.Cursor()
}

func (e *limitExec) Next(ctx context.Context) (value [][]byte, err error) {
	if e.cursor >= e.limit {
		return nil, nil
	}

	value, err = e.src.Next(ctx)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if value == nil {
		return nil, nil
	}
	e.cursor++
	return value, nil
}

func hasColVal(data [][]byte, colIDs map[int64]int, id int64) bool {
	offset, ok := colIDs[id]
	if ok && data[offset] != nil {
		return true
	}
	return false
}

// getRowData decodes raw byte slice to row data.
func getRowData(columns []*tipb.ColumnInfo, colIDs map[int64]int, handle int64, value []byte) ([][]byte, error) {
	values, err := tablecodec.CutRowNew(value, colIDs)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if values == nil {
		values = make([][]byte, len(colIDs))
	}
	// Fill the handle and null columns.
	for _, col := range columns {
		id := col.GetColumnId()
		offset := colIDs[id]
		if col.GetPkHandle() || id == model.ExtraHandleID {
			var handleDatum types.Datum
			if mysql.HasUnsignedFlag(uint(col.GetFlag())) {
				// PK column is Unsigned.
				handleDatum = types.NewUintDatum(uint64(handle))
			} else {
				handleDatum = types.NewIntDatum(handle)
			}
			handleData, err1 := codec.EncodeValue(nil, nil, handleDatum)
			if err1 != nil {
				return nil, errors.Trace(err1)
			}
			values[offset] = handleData
			continue
		}
		if hasColVal(values, colIDs, id) {
			continue
		}
		if len(col.DefaultVal) > 0 {
			values[offset] = col.DefaultVal
			continue
		}
		if mysql.HasNotNullFlag(uint(col.GetFlag())) {
			return nil, errors.Errorf("Miss column %d", id)
		}

		values[offset] = []byte{codec.NilFlag}
	}

	return values, nil
}

func convertToExprs(sc *stmtctx.StatementContext, fieldTps []*types.FieldType, pbExprs []*tipb.Expr) ([]expression.Expression, error) {
	exprs := make([]expression.Expression, 0, len(pbExprs))
	for _, expr := range pbExprs {
		e, err := expression.PBToExpr(expr, fieldTps, sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		exprs = append(exprs, e)
	}
	return exprs, nil
}

func decodeHandle(data []byte) (int64, error) {
	var h int64
	buf := bytes.NewBuffer(data)
	err := binary.Read(buf, binary.BigEndian, &h)
	return h, errors.Trace(err)
}
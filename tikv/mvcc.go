package tikv

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync/atomic"
	"time"

	"github.com/coocood/badger"
	"github.com/cznic/mathutil"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/util/codec"
)

// MVCCStore is a wrapper of badger.DB to provide MVCC functions.
type MVCCStore struct {
	db          *badger.DB
	writeWorker *writeWorker
}

// NewMVCCStore creates a new MVCCStore
func NewMVCCStore(db *badger.DB) *MVCCStore {
	store := &MVCCStore{
		db:          db,
		writeWorker: &writeWorker{db: db, wakeUp: make(chan struct{}, 1)},
	}
	go store.writeWorker.run()
	return store
}

func (store *MVCCStore) Get(regCtx *regionCtx, key []byte, startTS uint64) ([]byte, error) {
	var result valueResult
	err := store.db.View(func(txn *badger.Txn) error {
		g := &getter{txn: txn, regCtx: regCtx}
		defer g.close()
		result = g.get(key, startTS)
		return nil
	})
	if result.err == nil {
		result.err = errors.Trace(err)
	}
	return result.value, result.err
}

func newIterator(txn *badger.Txn) *badger.Iterator {
	var itOpts = badger.DefaultIteratorOptions
	itOpts.PrefetchValues = false
	return txn.NewIterator(itOpts)
}

type valueResult struct {
	commitTS uint64
	value    []byte
	err      error
}

type getter struct {
	txn    *badger.Txn
	regCtx *regionCtx
	iter   *badger.Iterator
}

func (g *getter) get(key []byte, startTS uint64) (result valueResult) {
	item, err := g.txn.Get(key)
	if err != nil && err != badger.ErrKeyNotFound {
		result.err = errors.Trace(err)
		return
	}
	if err == badger.ErrKeyNotFound {
		return
	}
	mixed, err1 := decodeMixed(item)
	if err1 != nil {
		result.err = errors.Trace(err)
		return
	}
	if mixed.hasLock() {
		result.err = checkLock(g.regCtx, mixed.lock, key, startTS)
		if result.err != nil {
			return
		}
	}
	if !mixed.hasValue() {
		return
	}
	mvVal := mixed.val
	if mvVal.commitTS <= startTS {
		result.commitTS = mvVal.commitTS
		result.value = mvVal.value
		return
	}
	oldKey := encodeOldKey(key, startTS)
	if g.iter == nil {
		g.iter = newIterator(g.txn)
	}
	g.iter.Seek(oldKey)
	if !g.iter.ValidForPrefix(oldKey[:len(oldKey)-8]) {
		return
	}
	item = g.iter.Item()
	mvVal, err = decodeValue(item)
	if err != nil {
		result.err = errors.Trace(err)
		return
	}
	result.commitTS = mvVal.commitTS
	result.value = mvVal.value
	return
}

func (g *getter) close() {
	if g.iter != nil {
		g.iter.Close()
	}
}

func checkLock(regCtx *regionCtx, lock mvccLock, key []byte, startTS uint64) error {
	lockVisible := lock.startTS < startTS
	isWriteLock := lock.op == kvrpcpb.Op_Put || lock.op == kvrpcpb.Op_Del
	isPrimaryGet := lock.startTS == lockVer && bytes.Equal(lock.primary, key)
	if lockVisible && isWriteLock && !isPrimaryGet {
		if extractPhysical(lock.startTS)+lock.ttl < extractPhysical(startTS) {
			regCtx.addTxnKey(lock.startTS, key)
		}
		return &ErrLocked{
			Key:     key,
			StartTS: lock.startTS,
			Primary: lock.primary,
			TTL:     lock.ttl,
		}
	}
	return nil
}

func extractPhysical(ts uint64) uint64 {
	return ts >> 18
}

func (store *MVCCStore) BatchGet(regCtx *regionCtx, keys [][]byte, startTS uint64) []Pair {
	var pairs []Pair
	err := store.db.View(func(txn *badger.Txn) error {
		g := &getter{txn: txn, regCtx: regCtx}
		defer g.close()
		for _, key := range keys {
			result := g.get(key, startTS)
			if len(result.value) == 0 {
				continue
			}
			pairs = append(pairs, Pair{Key: key, Value: result.value, Err: result.err})
		}
		return nil
	})
	if err != nil {
		log.Error(err)
		return []Pair{{Err: err}}
	}
	return pairs
}

func (store *MVCCStore) Prewrite(regCtx *regionCtx, mutations []*kvrpcpb.Mutation, primary []byte, startTS uint64, ttl uint64) []error {
	hashVals := mutationsToHashVals(mutations)
	store.acquireLocks(regCtx, hashVals)
	defer regCtx.releaseLocks(hashVals)
	errs := make([]error, 0, len(mutations))
	batch := &writeBatch{entries: make([]*badger.Entry, 0, len(mutations))}
	var anyError bool
	err := store.db.View(func(txn *badger.Txn) error {
		for _, m := range mutations {
			err1 := batch.prewriteMutation(regCtx, txn, m, primary, startTS, ttl)
			if err1 != nil {
				anyError = true
			}
			errs = append(errs, err1)
		}
		return nil
	})
	if err != nil {
		return []error{err}
	}
	if anyError {
		return errs
	}
	keys := make([][]byte, 0, len(mutations))
	for _, mu := range mutations {
		keys = append(keys, mu.Key)
	}
	regCtx.addTxnKeys(startTS, keys)
	err = store.write(batch)
	if err != nil {
		return []error{err}
	}
	return nil
}

const lockVer uint64 = math.MaxUint64

func (batch *writeBatch) prewriteMutation(regCtx *regionCtx, txn *badger.Txn, mutation *kvrpcpb.Mutation, primary []byte, startTS uint64, ttl uint64) error {
	item, err := txn.Get(mutation.Key)
	if err != nil && err != badger.ErrKeyNotFound {
		return errors.Trace(err)
	}
	var mixed mixedValue
	if item != nil {
		mixed, err = decodeMixed(item)
		if err != nil {
			return errors.Trace(err)
		}
		if mixed.hasLock() {
			lock := mixed.lock
			if lock.op != kvrpcpb.Op_Rollback {
				if lock.startTS != startTS {
					if extractPhysical(lock.startTS)+lock.ttl < extractPhysical(startTS) {
						regCtx.addTxnKey(lock.startTS, mutation.Key)
					}
					return ErrRetryable("key is locked, try again later")
				}
				// Same ts, no need to overwrite.
				return nil
			}
			// Rollback lock type
			if lock.startTS >= startTS {
				return ErrAbort("already rollback")
			}
			// If a rollback lock has a smaller start ts, we can overwrite it.
		}
		if mixed.hasValue() {
			mvVal := mixed.val
			if mvVal.commitTS > startTS {
				return ErrRetryable("write conflict")
			}
		}
	}
	mixed.lock = mvccLock{
		startTS: startTS,
		primary: primary,
		value:   mutation.Value,
		op:      mutation.Op,
		ttl:     ttl,
	}
	mixed.mixedType |= mixedLockFlag
	batch.setWithMeta(mutation.Key, mixed.MarshalBinary(), mixed.mixedType)
	return nil
}

// Commit implements the MVCCStore interface.
func (store *MVCCStore) Commit(regCtx *regionCtx, keys [][]byte, startTS, commitTS uint64, diff *int64) error {
	hashVals := keysToHashVals(keys)
	store.acquireLocks(regCtx, hashVals)
	defer regCtx.releaseLocks(hashVals)
	batch := new(writeBatch)
	var tmpDiff int64
	err := store.db.View(func(txn *badger.Txn) error {
		tmpDiff = 0
		for _, key := range keys {
			err1 := batch.commitKey(txn, key, startTS, commitTS, &tmpDiff)
			if err1 != nil {
				return err1
			}
		}
		return nil
	})
	if err != nil {
		return errors.Trace(err)
	}
	atomic.AddInt64(diff, tmpDiff)
	regCtx.removeTxnKeys(startTS)
	err = store.write(batch)
	return errors.Trace(err)
}

func (batch *writeBatch) commitKey(txn *badger.Txn, key []byte, startTS, commitTS uint64, diff *int64) error {
	item, err := txn.Get(key)
	if err != nil {
		return errors.Trace(err)
	}
	mixed, err := decodeMixed(item)
	if !mixed.hasLock() {
		if mixed.val.startTS == startTS {
			// Already committed.
			return nil
		} else {
			// The transaction may be committed and moved to old data, we need to look for that.
			oldKey := encodeOldKey(key, commitTS)
			_, err = txn.Get(oldKey)
			if err == nil {
				// Found committed key.
				return nil
			}
		}
		return errors.New("lock not found")
	}
	lock := mixed.lock
	if lock.startTS != startTS {
		return errors.New("replaced by another transaction")
	}
	if lock.op == kvrpcpb.Op_Rollback {
		return errors.New("already rollback")
	}
	batch.commitLock(txn, key, mixed, startTS, commitTS, diff)
	return nil
}

func (batch *writeBatch) commitLock(txn *badger.Txn, key []byte, mixed mixedValue, startTS, commitTS uint64, diff *int64) {
	lock := mixed.lock
	if lock.op == kvrpcpb.Op_Lock {
		batch.commitMixed(key, mixed, nil)
		return
	}
	if mixed.hasValue() {
		val := mixed.val
		oldDataKey := encodeOldKey(key, val.commitTS)
		batch.entries = append(batch.entries, &badger.Entry{Key: oldDataKey, Value: val.MarshalBinary()})
	}
	var valueType mvccValueType
	if lock.op == kvrpcpb.Op_Put {
		valueType = typePut
	} else {
		valueType = typeDelete
		mixed.mixedType |= mixedDelFlag
	}
	mixed.mixedType |= mixedValueFlag
	mixed.val = mvccValue{
		valueType: valueType,
		startTS:   startTS,
		commitTS:  commitTS,
		value:     lock.value,
	}
	batch.commitMixed(key, mixed, diff)
	return
}

func (batch *writeBatch) commitMixed(key []byte, mixed mixedValue, diff *int64) {
	rollbackTS := mixed.lock.rollbackTS
	if rollbackTS != 0 {
		// The rollback info is appended to the lock, we should reserve a rollback lock.
		mixed.lock = mvccLock{
			startTS: rollbackTS,
			op:      kvrpcpb.Op_Rollback,
		}
	} else {
		mixed.unsetLock()
	}
	mixedBin := mixed.MarshalBinary()
	if diff != nil {
		*diff += int64(len(key) + len(mixedBin))
	}
	batch.setWithMeta(key, mixed.MarshalBinary(), mixed.mixedType)
}

func (store *MVCCStore) Rollback(regCtx *regionCtx, keys [][]byte, startTS uint64) error {
	hashVals := keysToHashVals(keys)
	store.acquireLocks(regCtx, hashVals)
	defer regCtx.releaseLocks(hashVals)

	wb := new(writeBatch)
	err1 := store.db.View(func(txn *badger.Txn) error {
		for _, key := range keys {
			err := wb.rollbackKey(txn, key, startTS)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err1 != nil {
		log.Error(err1)
		return err1
	}
	regCtx.removeTxnKeys(startTS)
	return store.write(wb)
}

func (batch *writeBatch) rollbackKey(txn *badger.Txn, key []byte, startTS uint64) error {
	item, err := txn.Get(key)
	if err != nil && err != badger.ErrKeyNotFound {
		return errors.Trace(err)
	}
	if item == nil {
		// The prewrite request is not arrived, we write a rollback lock to prevent the future prewrite.
		mixed := mixedValue{
			mixedType: mixedLockFlag,
			lock: mvccLock{
				startTS: startTS,
				op:      kvrpcpb.Op_Rollback,
			}}
		batch.setWithMeta(key, mixed.MarshalBinary(), mixed.mixedType)
		return nil
	}
	mixed, err1 := decodeMixed(item)
	if err1 != nil {
		return errors.Trace(err1)
	}
	if mixed.hasLock() {
		lock := mixed.lock
		if lock.startTS < startTS {
			if lock.rollbackTS >= startTS {
				return nil
			}
			// The lock is old, means this is written by an old transaction, and the current transaction may not arrive.
			// We should append the startTS to the lock as rollbackTS.
			lock.rollbackTS = startTS
			batch.setWithMeta(key, mixed.MarshalBinary(), mixed.mixedType)
			return nil
		}
		if lock.startTS == startTS {
			if lock.op == kvrpcpb.Op_Rollback {
				return nil
			}
			// We can not simply delete the lock because the prewrite may be sent multiple times.
			// To prevent that we update it a rollback lock.
			mixed.lock = mvccLock{startTS: startTS, op: kvrpcpb.Op_Rollback}
			batch.setWithMeta(key, mixed.MarshalBinary(), mixed.mixedType)
			return nil
		}
	}
	if !mixed.hasValue() {
		return nil
	}
	val := mixed.val
	if val.startTS == startTS {
		return ErrAlreadyCommitted(val.commitTS)
	}
	if val.startTS < startTS {
		// Prewrite and commit have not arrived.
		mixed.lock = mvccLock{startTS: startTS, op: kvrpcpb.Op_Rollback}
		mixed.mixedType |= mixedLockFlag
		batch.setWithMeta(key, mixed.MarshalBinary(), mixed.mixedType)
		return nil
	}
	// Look for the key in the old version.
	iter := newIterator(txn)
	oldKey := encodeOldKey(key, val.commitTS)
	// find greater commit version.
	for iter.Seek(oldKey); iter.ValidForPrefix(oldKey[:len(oldKey)-8]); iter.Next() {
		item := iter.Item()
		foundKey := item.Key()
		if isVisibleKey(foundKey, startTS) {
			break
		}
		_, ts, err := codec.DecodeUintDesc(foundKey[len(foundKey)-8:])
		if err != nil {
			return errors.Trace(err)
		}
		mvVal, err := decodeValue(item)
		if mvVal.startTS == startTS {
			return ErrAlreadyCommitted(ts)
		}
	}
	return nil
}

func (store *MVCCStore) Scan(regCtx *regionCtx, startKey, endKey []byte, limit int, startTS uint64) []Pair {
	var pairs []Pair
	err := store.db.View(func(txn *badger.Txn) error {
		iter := newIterator(txn)
		defer iter.Close()
		var oldIter *badger.Iterator
		for iter.Seek(startKey); iter.Valid(); iter.Next() {
			item := iter.Item()
			if exceedEndKey(item.Key(), endKey) {
				return nil
			}
			mixed, err1 := decodeMixed(item)
			if err1 != nil {
				return errors.Trace(err1)
			}
			key := item.KeyCopy(nil)
			if mixed.hasLock() {
				err1 = checkLock(regCtx, mixed.lock, key, startTS)
				if err1 != nil {
					return errors.Trace(err1)
				}
			}
			if !mixed.hasValue() {
				continue
			}
			mvVal := mixed.val
			if mvVal.commitTS > startTS {
				if oldIter == nil {
					oldIter = newIterator(txn)
				}
				mvVal, err1 = store.getOldValue(oldIter, encodeOldKey(item.Key(), startTS))
				if err1 == badger.ErrKeyNotFound {
					continue
				}
			}
			if mvVal.valueType == typeDelete {
				continue
			}
			pairs = append(pairs, Pair{Key: key, Value: mvVal.value})
			if len(pairs) >= limit {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return []Pair{{Err: err}}
	}
	return pairs
}

func (store *MVCCStore) getOldValue(oldIter *badger.Iterator, oldKey []byte) (mvccValue, error) {
	oldIter.Seek(oldKey)
	if !oldIter.ValidForPrefix(oldKey[:len(oldKey)-8]) {
		return mvccValue{}, badger.ErrKeyNotFound
	}
	return decodeValue(oldIter.Item())
}

func isVisibleKey(key []byte, startTS uint64) bool {
	ts := ^(binary.BigEndian.Uint64(key[len(key)-8:]))
	return startTS >= ts
}

// ReverseScan implements the MVCCStore interface. The search range is [startKey, endKey).
func (store *MVCCStore) ReverseScan(regCtx *regionCtx, startKey, endKey []byte, limit int, startTS uint64) []Pair {
	var pairs []Pair
	err := store.db.View(func(txn *badger.Txn) error {
		var opts badger.IteratorOptions
		opts.Reverse = true
		opts.PrefetchValues = false
		iter := txn.NewIterator(opts)
		defer iter.Close()
		var oldIter *badger.Iterator
		for iter.Seek(endKey); iter.Valid(); iter.Next() {
			item := iter.Item()
			if bytes.Compare(item.Key(), startKey) < 0 {
				return nil
			}
			mixed, err1 := decodeMixed(item)
			if err1 != nil {
				return errors.Trace(err1)
			}
			key := item.KeyCopy(nil)
			if err1 != nil {
				return errors.Trace(err1)
			}
			if mixed.hasLock() {
				err1 = checkLock(regCtx, mixed.lock, key, startTS)
				if err1 != nil {
					return errors.Trace(err1)
				}
			}
			if !mixed.hasValue() {
				continue
			}
			mvVal := mixed.val
			if mvVal.commitTS > startTS {
				if oldIter == nil {
					oldIter = newIterator(txn)
				}
				mvVal, err1 = store.getOldValue(oldIter, encodeOldKey(item.Key(), startTS))
				if err1 == badger.ErrKeyNotFound {
					continue
				}
			}
			if mvVal.valueType == typeDelete {
				continue
			}
			pairs = append(pairs, Pair{Key: key, Value: mvVal.value})
			if len(pairs) >= limit {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return []Pair{{Err: err}}
	}
	return pairs
}

func (store *MVCCStore) Cleanup(regCtx *regionCtx, key []byte, startTS uint64) error {
	hashVals := keysToHashVals([][]byte{key})
	store.acquireLocks(regCtx, hashVals)
	defer regCtx.releaseLocks(hashVals)
	wb := new(writeBatch)
	err := store.db.View(func(txn *badger.Txn) error {
		return wb.rollbackKey(txn, key, startTS)
	})
	if err != nil {
		return err
	}
	regCtx.removeTxnKey(startTS, key)
	store.write(wb)
	return err
}

func (store *MVCCStore) ScanLock(regCtx *regionCtx, maxTS uint64) ([]*kvrpcpb.LockInfo, error) {
	var locks []*kvrpcpb.LockInfo
	allKeys := regCtx.getAllKeys(maxTS)
	err1 := store.db.View(func(txn *badger.Txn) error {
		for _, key := range allKeys {
			item, err := txn.Get(key)
			if err == badger.ErrKeyNotFound {
				continue
			}
			if err != nil {
				return errors.Trace(err)
			}
			mixed, err := decodeMixed(item)
			if err != nil {
				return errors.Trace(err)
			}
			if !mixed.hasLock() {
				continue
			}
			lock := mixed.lock
			if lock.op == kvrpcpb.Op_Rollback {
				continue
			}
			if lock.startTS < maxTS {
				locks = append(locks, &kvrpcpb.LockInfo{
					PrimaryLock: lock.primary,
					LockVersion: lock.startTS,
					Key:         codec.EncodeBytes(nil, item.Key()),
					LockTtl:     lock.ttl,
				})
			}
		}
		return nil
	})
	if err1 != nil {
		log.Error(err1)
	}
	return locks, nil
}

func (store *MVCCStore) ResolveLock(regCtx *regionCtx, startTS, commitTS uint64, diff *int64) error {
	lockKeys := regCtx.getTxnKeys(startTS)
	if len(lockKeys) == 0 {
		log.Debugf("no lock keys found for startTS:%d, commitTS:%d", startTS, commitTS)
		return nil
	}
	hashVals := keysToHashVals(lockKeys)
	store.acquireLocks(regCtx, hashVals)
	defer regCtx.releaseLocks(hashVals)
	wb := new(writeBatch)
	var tmpDiff int64
	err := store.db.View(func(txn *badger.Txn) error {
		iter := newIterator(txn)
		defer iter.Close()
		for _, key := range lockKeys {
			item, err := txn.Get(key)
			if err == badger.ErrKeyNotFound {
				continue
			}
			if err != nil {
				return errors.Trace(err)
			}
			mixed, err := decodeMixed(item)
			if err != nil {
				return errors.Trace(err)
			}
			if !mixed.hasLock() {
				continue
			}
			lock := mixed.lock
			if lock.startTS != startTS {
				continue
			}
			if commitTS > 0 {
				err = wb.commitKey(txn, key, startTS, commitTS, &tmpDiff)
			} else {
				err = wb.rollbackKey(txn, key, startTS)
			}
			if err != nil {
				return errors.Trace(err)
			}
		}
		return nil
	})
	if err != nil {
		log.Errorf("resolve lock failed with %d locks, %v", len(lockKeys), err)
		return errors.Trace(err)
	}
	if len(wb.entries) == 0 {
		return nil
	}
	atomic.AddInt64(diff, tmpDiff)
	regCtx.removeTxnKeys(startTS)
	return store.write(wb)
}

const delRangeBatchSize = 4096

func (store *MVCCStore) DeleteRange(regCtx *regionCtx, startKey, endKey []byte) error {
	keys := make([][]byte, 0, delRangeBatchSize)
	oldStartKey := encodeOldKey(startKey, lockVer)
	oldEndKey := encodeOldKey(endKey, lockVer)

	err := store.db.View(func(txn *badger.Txn) error {
		iter := newIterator(txn)
		defer iter.Close()
		keys = store.collectRangeKeys(iter, startKey, endKey, keys)
		keys = store.collectRangeKeys(iter, oldStartKey, oldEndKey, keys)
		return nil
	})
	if err != nil {
		log.Error(err)
		return errors.Trace(err)
	}
	err = store.deleteKeysInBatch(regCtx, keys, delRangeBatchSize)
	if err != nil {
		log.Error(err)
	}
	return errors.Trace(err)
}

func (store *MVCCStore) collectRangeKeys(iter *badger.Iterator, startKey, endKey []byte, keys [][]byte) [][]byte {
	for iter.Seek(startKey); iter.Valid(); iter.Next() {
		item := iter.Item()
		key := item.KeyCopy(nil)
		if exceedEndKey(key, endKey) {
			break
		}
		keys = append(keys, key)
		if len(keys) == delRangeBatchSize {
			break
		}
	}
	return keys
}

func (store *MVCCStore) deleteKeysInBatch(regCtx *regionCtx, keys [][]byte, batchSize int) error {
	for len(keys) > 0 {
		batchSize := mathutil.Min(len(keys), batchSize)
		batchKeys := keys[:batchSize]
		keys = keys[batchSize:]

		hashVals := keysToHashVals(batchKeys)
		store.acquireLocks(regCtx, hashVals)
		wb := new(writeBatch)
		err := store.db.View(func(txn *badger.Txn) error {
			for _, key := range batchKeys {
				wb.delete(key)
			}
			return nil
		})
		if err != nil {
			log.Error(err)
			regCtx.releaseLocks(hashVals)
			return errors.Trace(err)
		}
		err = store.write(wb)
		regCtx.releaseLocks(hashVals)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

const gcBatchSize = 256

func (store *MVCCStore) GC(regCtx *regionCtx, safePoint uint64) error {
	err := store.gcOldVersions(regCtx, safePoint)
	if err != nil {
		return errors.Trace(err)
	}

	err = store.gcDelAndRollbacks(regCtx, safePoint)
	return errors.Trace(err)
}

func (store *MVCCStore) gcOldVersions(regCtx *regionCtx, safePoint uint64) error {
	var gcKeys [][]byte
	err := store.db.View(func(txn *badger.Txn) error {
		iter := newIterator(txn)
		defer iter.Close()
		oldStartKey := encodeOldKey(regCtx.startKey, lockVer)
		oldEndKey := encodeOldKey(regCtx.endKey, lockVer)
		for iter.Seek(oldStartKey); iter.Valid(); iter.Next() {
			item := iter.Item()
			if exceedEndKey(item.Key(), oldEndKey) {
				return nil
			}
			key := item.Key()
			_, ts, err1 := codec.DecodeUintDesc(key[len(key)-8:])
			if err1 != nil {
				return errors.Trace(err1)
			}
			if ts <= safePoint {
				gcKeys = append(gcKeys, item.KeyCopy(nil))
			}
		}
		return nil
	})
	if err != nil {
		return errors.Trace(err)
	}
	log.Debugf("gc old keys %d", len(gcKeys))
	err = store.deleteKeysInBatch(regCtx, gcKeys, gcBatchSize)
	return errors.Trace(err)
}

func (store *MVCCStore) gcDelAndRollbacks(regCtx *regionCtx, safePoint uint64) error {
	var gcKeys [][]byte
	var gcKeyVers []uint64
	err := store.db.View(func(txn *badger.Txn) error {
		iter := newIterator(txn)
		defer iter.Close()
		for iter.Seek(regCtx.startKey); iter.Valid(); iter.Next() {
			item := iter.Item()
			if exceedEndKey(item.Key(), regCtx.endKey) {
				return nil
			}
			flag := item.UserMeta()
			if flag&mixedDelFlag > 0 || flag&mixedLockFlag > 0 {
				mixed, err := decodeMixed(item)
				if err != nil {
					return errors.Trace(err)
				}
				if mixed.hasLock() && !mixed.hasValue() {
					lock := mixed.lock
					if lock.op == kvrpcpb.Op_Rollback && lock.startTS <= safePoint {
						gcKeys = append(gcKeys, item.KeyCopy(nil))
						gcKeyVers = append(gcKeyVers, item.Version())
					}
				} else if mixed.isDelete() {
					if mixed.val.commitTS <= safePoint {
						gcKeys = append(gcKeys, item.KeyCopy(nil))
						gcKeyVers = append(gcKeyVers, item.Version())
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return errors.Trace(err)
	}
	log.Debugf("gc delete keys %d", len(gcKeys))
	err = store.gcDelKeysInBatch(regCtx, gcKeys, gcKeyVers)
	return errors.Trace(err)
}

func (store *MVCCStore) gcDelKeysInBatch(regCtx *regionCtx, keys [][]byte, keyVers []uint64) error {
	for len(keys) > 0 {
		batchSize := mathutil.Min(len(keys), gcBatchSize)
		batchKeys := keys[:batchSize]
		batchKeyVers := keyVers[:batchSize]
		keys = keys[batchSize:]
		keyVers = keyVers[:batchSize]

		hashVals := keysToHashVals(batchKeys)
		store.acquireLocks(regCtx, hashVals)
		wb := new(writeBatch)
		err := store.db.View(func(txn *badger.Txn) error {
			for i, key := range batchKeys {
				item, err1 := txn.Get(key)
				if err1 == badger.ErrKeyNotFound {
					continue
				}
				if err1 != nil {
					return errors.Trace(err1)
				}
				if item.Version() != batchKeyVers[i] {
					continue
				}
				wb.delete(key)
			}
			return nil
		})
		if err != nil {
			regCtx.releaseLocks(hashVals)
			log.Error(err)
			return errors.Trace(err)
		}
		err = store.write(wb)
		regCtx.releaseLocks(hashVals)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

// Pair is a KV pair read from MvccStore or an error if any occurs.
type Pair struct {
	Key   []byte
	Value []byte
	Err   error
}

func (store *MVCCStore) acquireLocks(regCtx *regionCtx, hashVals []uint64) {
	start := time.Now()
	for {
		ok, wg, lockLen := regCtx.acquireLocks(hashVals)
		if ok {
			dur := time.Since(start)
			if dur > time.Millisecond*50 {
				log.Warnf("acquire %d locks takes %v, memLock size %d", len(hashVals), dur, lockLen)
			}
			return
		}
		wg.Wait()
	}
}
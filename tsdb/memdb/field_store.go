package memdb

import (
	"sort"

	"github.com/eleme/lindb/pkg/field"
	"github.com/eleme/lindb/pkg/logger"
	"github.com/eleme/lindb/pkg/timeutil"
	pb "github.com/eleme/lindb/rpc/proto/field"
	"github.com/eleme/lindb/tsdb/metrictbl"
)

//go:generate mockgen -source ./field_store.go -destination=./field_store_mock_test.go -package memdb

// fStoreINTF abstracts a field-store
type fStoreINTF interface {
	// getFieldName returns the name of the field
	getFieldName() string
	// getFieldType returns the field-type
	getFieldType() field.Type
	// write writes the metric's field with writeContext
	write(f *pb.Field, writeCtx writeContext)
	// flushFieldTo flushes field data of the specific familyTime
	// return false if there is no data related of familyTime
	flushFieldTo(tableFlusher metrictbl.TableFlusher, familyTime int64) (flushed bool)
	// timeRange returns the start-time and end-time of fStore's data
	// ok means data is available
	timeRange(interval int64) (timeRange timeutil.TimeRange, ok bool)
}

// sStoreNodes implements the sort.Interface
type sStoreNodes []sStoreINTF

func (s sStoreNodes) Len() int           { return len(s) }
func (s sStoreNodes) Less(i, j int) bool { return s[i].getFamilyTime() < s[j].getFamilyTime() }
func (s sStoreNodes) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// fieldStore holds the relation of familyStartTime and segmentStore.
// there are only a few familyTimes in the segments,
// add delete operation occurs every one hour
// so slice is more cheaper than the map
type fieldStore struct {
	fieldName   string
	fieldType   field.Type  // sum, gauge, min, max
	fieldID     uint16      // generated by id generator
	sStoreNodes sStoreNodes // sorted sStore list by family-time
}

// newFieldStore returns a new fieldStore.
func newFieldStore(fieldName string, fieldID uint16, fieldType field.Type) fStoreINTF {
	return &fieldStore{
		fieldName: fieldName,
		fieldID:   fieldID,
		fieldType: fieldType}
}

/// getFieldName returns the name of the field
func (fs *fieldStore) getFieldName() string {
	return fs.fieldName
}

// getSStore gets the sStore from list by familyTime.
func (fs *fieldStore) getSStore(familyTime int64) (sStoreINTF, bool) {
	idx := sort.Search(len(fs.sStoreNodes), func(i int) bool {
		return fs.sStoreNodes[i].getFamilyTime() >= familyTime
	})
	if idx >= len(fs.sStoreNodes) || fs.sStoreNodes[idx].getFamilyTime() != familyTime {
		return nil, false
	}
	return fs.sStoreNodes[idx], true
}

// removeSStore removes the sStore by familyTime.
func (fs *fieldStore) removeSStore(familyTime int64) {
	idx := sort.Search(len(fs.sStoreNodes), func(i int) bool {
		return fs.sStoreNodes[i].getFamilyTime() >= familyTime
	})
	// familyTime greater than existed
	if idx == len(fs.sStoreNodes) {
		return
	}
	// not match
	if fs.sStoreNodes[idx].getFamilyTime() != familyTime {
		return
	}
	copy(fs.sStoreNodes[idx:], fs.sStoreNodes[idx+1:])
	// fills the tail with nil
	fs.sStoreNodes[len(fs.sStoreNodes)-1] = nil
	fs.sStoreNodes = fs.sStoreNodes[:len(fs.sStoreNodes)-1]
}

// insertSStore inserts a new sStore to segments.
func (fs *fieldStore) insertSStore(sStore sStoreINTF) {
	fs.sStoreNodes = append(fs.sStoreNodes, sStore)
	sort.Sort(fs.sStoreNodes)
}

// getFieldType returns field type for current field store
func (fs *fieldStore) getFieldType() field.Type {
	return fs.fieldType
}

func (fs *fieldStore) write(f *pb.Field, writeCtx writeContext) {
	sStore, ok := fs.getSStore(writeCtx.familyTime)

	switch fields := f.Field.(type) {
	case *pb.Field_Sum:
		if !ok {
			//TODO ???
			sStore = newSimpleFieldStore(writeCtx.familyTime, field.GetAggFunc(field.Sum))
			fs.insertSStore(sStore)
		}
		sStore.writeFloat(fields.Sum, writeCtx)
	default:
		memDBLogger.Warn("convert field error, unknown field type")
	}
}

// flushFieldTo flushes segments' data to writer and reset the segments-map.
func (fs *fieldStore) flushFieldTo(tableFlusher metrictbl.TableFlusher, familyTime int64) (flushed bool) {
	sStore, ok := fs.getSStore(familyTime)

	if !ok {
		return false
	}

	fs.removeSStore(familyTime)
	data, startSlot, endSlot, err := sStore.bytes()

	if err != nil {
		memDBLogger.Error("read segment data error:", logger.Error(err))
		return false
	}
	tableFlusher.FlushField(fs.fieldID, fs.fieldType, data, startSlot, endSlot)
	return true
}

func (fs *fieldStore) timeRange(interval int64) (timeRange timeutil.TimeRange, ok bool) {
	for _, sStore := range fs.sStoreNodes {
		startSlot, endSlot, err := sStore.slotRange()
		if err != nil {
			continue
		}
		ok = true
		startTime := sStore.getFamilyTime() + int64(startSlot)*interval
		endTime := sStore.getFamilyTime() + int64(endSlot)*interval
		if timeRange.Start == 0 || startTime < timeRange.Start {
			timeRange.Start = startTime
		}
		if timeRange.End == 0 || timeRange.End < endTime {
			timeRange.End = endTime
		}
	}
	return
}

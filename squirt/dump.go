package squirt

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strconv"
)

var packageNameStripperRegexp = regexp.MustCompile("\\b[a-zA-Z_]+[a-zA-Z_0-9]+\\.")

type dumpConfig struct {
	collectionTypes   bool
	stripPackageNames bool
}

type dumpState struct {
	w                  io.Writer
	depth              int
	config             dumpConfig
	pointers           []uintptr
	visitedPointers    []uintptr
	currentPointerName string
}

func (s *dumpState) indent() {
	s.w.Write(bytes.Repeat([]byte("  "), s.depth))
}

func (s *dumpState) newlineWithPointerNameComment() {
	if s.currentPointerName != "" {
		s.w.Write([]byte(fmt.Sprintf(" // %s\n", s.currentPointerName)))
		s.currentPointerName = ""
		return
	}
	s.w.Write([]byte("\n"))
}

func (s *dumpState) dumpType(v reflect.Value) {
	typeName := v.Type().String()
	if s.config.stripPackageNames {
		typeName = packageNameStripperRegexp.ReplaceAllLiteralString(typeName, "")
	}
	s.w.Write([]byte(typeName))
}

func (s *dumpState) dumpSlice(v reflect.Value) {
	s.dumpType(v)
	numEntries := v.Len()
	if numEntries == 0 {
		s.w.Write([]byte("{}"))
		s.newlineWithPointerNameComment()
		return
	}
	s.w.Write([]byte("{"))
	s.newlineWithPointerNameComment()
	s.depth++
	for i := 0; i < numEntries; i++ {
		s.indent()
		s.dumpVal(v.Index(i))
		s.w.Write([]byte(","))
		s.newlineWithPointerNameComment()
	}
	s.depth--
	s.indent()
	s.w.Write([]byte("}"))
}

func (s *dumpState) dumpStruct(v reflect.Value) {
	s.dumpType(v)
	s.w.Write([]byte("{"))
	s.newlineWithPointerNameComment()
	s.depth++
	vt := v.Type()
	numFields := v.NumField()
	for i := 0; i < numFields; i++ {
		s.indent()
		vtf := vt.Field(i)
		s.w.Write([]byte(vtf.Name))
		s.w.Write([]byte(": "))
		s.dumpVal(v.Field(i))
		s.w.Write([]byte(","))
		s.newlineWithPointerNameComment()
	}
	s.depth--
	s.indent()
	s.w.Write([]byte("}"))
}

func (s *dumpState) dumpMap(v reflect.Value) {
	s.dumpType(v)
	s.w.Write([]byte("{"))
	s.newlineWithPointerNameComment()
	s.depth++
	keys := v.MapKeys()
	for _, key := range keys {
		s.indent()
		s.dumpVal(key)
		s.w.Write([]byte(": "))
		s.dumpVal(v.MapIndex(key))
		s.w.Write([]byte(","))
		s.newlineWithPointerNameComment()
	}
	s.depth--
	s.indent()
	s.w.Write([]byte("}"))
}

func (s *dumpState) dump(value interface{}) {
	if value == nil {
		printNil(s.w)
		return
	}
	v := reflect.ValueOf(value)
	s.dumpVal(v)
}

func (s *dumpState) handlePointerAliasingAndCheckIfShouldDescend(value reflect.Value) bool {
	pointerName, firstVisit := s.pointerNameFor(value)
	if pointerName == "" {
		return true
	}
	if firstVisit {
		s.currentPointerName = pointerName
		return true
	}
	s.w.Write([]byte(pointerName))
	return false
}

func (s *dumpState) dumpVal(value reflect.Value) {
	v := deInterface(value)
	kind := v.Kind()
	switch kind {
	case reflect.Invalid:
		// Do nothing.  We should never get here since invalid has already
		// been handled above.

	case reflect.Bool:
		printBool(s.w, v.Bool())

	case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Int:
		printInt(s.w, v.Int(), 10)

	case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		printUint(s.w, v.Uint(), 10)

	case reflect.Float32:
		printFloat(s.w, v.Float(), 32)

	case reflect.Float64:
		printFloat(s.w, v.Float(), 64)

	case reflect.Complex64:
		printComplex(s.w, v.Complex(), 32)

	case reflect.Complex128:
		printComplex(s.w, v.Complex(), 64)

	case reflect.String:
		s.w.Write([]byte(strconv.Quote(v.String())))

	case reflect.Slice:
		if v.IsNil() {
			printNil(s.w)
			break
		}
		fallthrough

	case reflect.Array:
		if s.handlePointerAliasingAndCheckIfShouldDescend(v) {
			s.dumpSlice(v)
		}

	case reflect.Interface:
		// The only time we should get here is for nil interfaces due to
		// unpackValue calls.
		if v.IsNil() {
			printNil(s.w)
		}

	case reflect.Ptr:
		// s.w.Write([]byte("&"))
		if s.handlePointerAliasingAndCheckIfShouldDescend(v) {
			s.dumpVal(v.Elem())
		}

	case reflect.Map:
		if s.handlePointerAliasingAndCheckIfShouldDescend(v) {
			s.dumpMap(v)
		}

	case reflect.Struct:
		s.dumpStruct(v)

	default:
		if v.CanInterface() {
			fmt.Fprintf(s.w, "%v", v.Interface())
		} else {
			fmt.Fprintf(s.w, "%v", v.String())
		}
	}
}

// call to signal that the pointer is being visited, returns true if this is the
// first visit to that pointer. Used to detect when to output the entire contents
// behind a pointer (the first time), and when to just emit a name (all other times)
func (s *dumpState) visitPointerAndCheckIfItIsTheFirstTime(ptr uintptr) bool {
	for _, addr := range s.visitedPointers {
		if addr == ptr {
			return false
		}
	}
	s.visitedPointers = append(s.visitedPointers, ptr)
	return true
}

// registers that the value has been visited and checks to see if it is one of the
// pointers we will see multiple times. If it is, it returns a temporary name for this
// pointer. It also returns a boolean value indicating whether this is the first time
// this name is returned so the caller can decide whether the contents of the pointer
// has been dumped before or not.
func (s *dumpState) pointerNameFor(v reflect.Value) (string, bool) {
	if isPointerValue(v) {
		ptr := v.Pointer()
		for i, addr := range s.pointers {
			if ptr == addr {
				firstVisit := s.visitPointerAndCheckIfItIsTheFirstTime(ptr)
				return fmt.Sprintf("p%d", i), firstVisit
			}
		}
	}
	return "", false
}

// Dump a value to stdout
func Dump(value interface{}) {
	state := &dumpState{
		w: os.Stdout,
		config: dumpConfig{
			stripPackageNames: true,
		},
		pointers: MapReusedPointers(reflect.ValueOf(value)),
	}
	state.dump(value)
	state.w.Write([]byte("\n"))
}

// Sdump dumps a value to a string
func Sdump(value interface{}) string {
	buf := new(bytes.Buffer)
	state := &dumpState{
		w: os.Stdout,
	}
	state.dump(value)
	return buf.String()
}

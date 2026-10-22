package pw

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

var le = binary.LittleEndian

// SPA POD value types (spa/utils/type.h).
const (
	typeNone      = 1
	typeBool      = 2
	typeID        = 3
	typeInt       = 4
	typeLong      = 5
	typeString    = 8
	typeRectangle = 10
	typeFraction  = 11
	typeStruct    = 14
	typeObject    = 15
	typeFd        = 18
	typeChoice    = 19
)

// SPA choice kinds (spa/pod/pod.h).
const (
	choiceNone  = 0
	choiceRange = 1
	choiceEnum  = 3
)

// builder serializes SPA PODs. Every POD is a (size, type) header and a body
// padded to 8 bytes; containers hold their children back to back.
type builder struct {
	b      []byte
	frames []int // start offsets of open containers
}

func (w *builder) bytes() []byte { return w.b }

func (w *builder) header(size, typ uint32) {
	w.b = le.AppendUint32(w.b, size)
	w.b = le.AppendUint32(w.b, typ)
}

func (w *builder) pad() {
	for len(w.b)%8 != 0 {
		w.b = append(w.b, 0)
	}
}

func (w *builder) word(typ, v uint32) *builder {
	w.header(4, typ)
	w.b = le.AppendUint32(w.b, v)
	w.pad()
	return w
}

func (w *builder) Int(v int32) *builder { return w.word(typeInt, uint32(v)) }
func (w *builder) ID(v uint32) *builder { return w.word(typeID, v) }

func (w *builder) Bool(v bool) *builder {
	if v {
		return w.word(typeBool, 1)
	}
	return w.word(typeBool, 0)
}

func (w *builder) Long(v int64) *builder {
	w.header(8, typeLong)
	w.b = le.AppendUint64(w.b, uint64(v))
	return w
}

func (w *builder) String(s string) *builder {
	w.header(uint32(len(s)+1), typeString)
	w.b = append(append(w.b, s...), 0)
	w.pad()
	return w
}

// Raw appends an already serialized POD.
func (w *builder) Raw(pod []byte) *builder {
	w.b = append(w.b, pod...)
	w.pad()
	return w
}

// push opens a container; its size is patched in by pop.
func (w *builder) push(typ uint32) {
	w.frames = append(w.frames, len(w.b))
	w.header(0, typ)
}

func (w *builder) pop() *builder {
	start := w.frames[len(w.frames)-1]
	w.frames = w.frames[:len(w.frames)-1]
	le.PutUint32(w.b[start:], uint32(len(w.b)-start-8))
	w.pad()
	return w
}

// Struct writes a struct whose fields f appends.
func (w *builder) Struct(f func()) *builder {
	w.push(typeStruct)
	f()
	return w.pop()
}

// Object writes an object of type typ and param id; f appends its properties
// with Prop followed by one value each.
func (w *builder) Object(typ, id uint32, f func()) *builder {
	w.push(typeObject)
	w.b = le.AppendUint32(w.b, typ)
	w.b = le.AppendUint32(w.b, id)
	f()
	return w.pop()
}

// Prop starts an object property; the next POD written is its value.
func (w *builder) Prop(key uint32) *builder {
	w.b = le.AppendUint32(w.b, key)
	w.b = le.AppendUint32(w.b, 0)
	return w
}

// Choice writes a choice of kind over values of one fixed-size type. For
// enums the first value is the default; ranges are (default, min, max).
func (w *builder) Choice(kind, typ uint32, values ...[]byte) *builder {
	size := uint32(len(values[0]))
	w.header(16+size*uint32(len(values)), typeChoice)
	w.b = le.AppendUint32(w.b, kind)
	w.b = le.AppendUint32(w.b, 0)
	w.b = le.AppendUint32(w.b, size)
	w.b = le.AppendUint32(w.b, typ)
	for _, v := range values {
		w.b = append(w.b, v...)
	}
	w.pad()
	return w
}

func idValue(v uint32) []byte          { return le.AppendUint32(nil, v) }
func fractionValue(n, d uint32) []byte { return le.AppendUint32(le.AppendUint32(nil, n), d) }

// Dict writes a PipeWire property dictionary: a struct of the item count
// followed by key/value strings.
func (w *builder) Dict(kv []string) *builder {
	return w.Struct(func() {
		w.Int(int32(len(kv) / 2))
		for _, s := range kv {
			w.String(s)
		}
	})
}

// pod is one parsed POD: its type and body.
type pod struct {
	typ  uint32
	body []byte
}

var errShort = errors.New("truncated SPA POD")

// readPod splits the first POD off b, skipping its padding.
func readPod(b []byte) (pod, []byte, error) {
	if len(b) < 8 {
		return pod{}, nil, errShort
	}
	size, typ := le.Uint32(b), le.Uint32(b[4:])
	if uint64(size) > uint64(len(b)-8) {
		return pod{}, nil, errShort
	}
	p := pod{typ: typ, body: b[8 : 8+size]}
	next := min(8+int((size+7)&^7), len(b))
	return p, b[next:], nil
}

// raw serializes the POD again, copying it out of the read buffer.
func (p pod) raw() []byte {
	b := le.AppendUint32(make([]byte, 0, 8+len(p.body)), uint32(len(p.body)))
	return append(le.AppendUint32(b, p.typ), p.body...)
}

// value unwraps a choice to its default value; other PODs are returned as is.
func (p pod) value() pod {
	if p.typ != typeChoice || len(p.body) < 16 {
		return p
	}
	size, typ := le.Uint32(p.body[8:]), le.Uint32(p.body[12:])
	if uint64(len(p.body)) < 16+uint64(size) {
		return pod{}
	}
	return pod{typ: typ, body: p.body[16 : 16+size]}
}

func (p pod) uint32Of(typ uint32) (uint32, bool) {
	p = p.value()
	if p.typ != typ || len(p.body) < 4 {
		return 0, false
	}
	return le.Uint32(p.body), true
}

// pair reads a Rectangle or Fraction.
func (p pod) pair(typ uint32) (uint32, uint32, bool) {
	p = p.value()
	if p.typ != typ || len(p.body) < 8 {
		return 0, 0, false
	}
	return le.Uint32(p.body), le.Uint32(p.body[4:]), true
}

// object returns an object's type, id and properties by key.
func (p pod) object() (typ, id uint32, props map[uint32]pod, err error) {
	if p.typ != typeObject || len(p.body) < 8 {
		return 0, 0, nil, errors.New("SPA POD is not an object")
	}
	typ, id = le.Uint32(p.body), le.Uint32(p.body[4:])
	props = map[uint32]pod{}
	rest := p.body[8:]
	for len(rest) >= 8 {
		key := le.Uint32(rest)
		var v pod
		if v, rest, err = readPod(rest[8:]); err != nil {
			return 0, 0, nil, err
		}
		props[key] = v
	}
	return typ, id, props, nil
}

// fields reads the members of a struct POD in order. The first error sticks,
// so callers read every field and check Err once.
type fields struct {
	rest []byte
	err  error
}

func structFields(p pod) *fields {
	if p.typ != typeStruct {
		return &fields{err: errors.New("SPA POD is not a struct")}
	}
	return &fields{rest: p.body}
}

// parseStruct reads a message body, which is a single struct POD.
func parseStruct(b []byte) *fields {
	p, _, err := readPod(b)
	if err != nil {
		return &fields{err: err}
	}
	return structFields(p)
}

func (f *fields) Err() error { return f.err }

func (f *fields) next(want uint32) pod {
	if f.err != nil {
		return pod{}
	}
	var p pod
	p, f.rest, f.err = readPod(f.rest)
	if f.err == nil && want != 0 && p.typ != want {
		f.err = fmt.Errorf("SPA POD type %d, want %d", p.typ, want)
	}
	return p
}

func (f *fields) word(typ uint32) uint32 {
	p := f.next(typ)
	if f.err != nil || len(p.body) < 4 {
		return 0
	}
	return le.Uint32(p.body)
}

func (f *fields) Int() int32   { return int32(f.word(typeInt)) }
func (f *fields) Uint() uint32 { return f.word(typeInt) }
func (f *fields) ID() uint32   { return f.word(typeID) }

func (f *fields) Long() int64 {
	p := f.next(typeLong)
	if f.err != nil || len(p.body) < 8 {
		return 0
	}
	return int64(le.Uint64(p.body))
}

// Fd returns the index of a file descriptor in the message's fd list.
func (f *fields) Fd() int {
	p := f.next(typeFd)
	if f.err != nil || len(p.body) < 8 {
		return -1
	}
	v := int64(le.Uint64(p.body))
	if v < 0 || v > math.MaxInt32 {
		return -1
	}
	return int(v)
}

func (f *fields) String() string {
	p := f.next(0)
	if f.err != nil || p.typ != typeString || len(p.body) == 0 {
		return "" // a None POD stands for a NULL string
	}
	return string(p.body[:len(p.body)-1])
}

// Pod returns the next field whatever its type; an object or None is
// expected where PipeWire sends an optional param.
func (f *fields) Pod() pod { return f.next(0) }

// Struct returns the members of a nested struct.
func (f *fields) Struct() *fields {
	p := f.next(typeStruct)
	if f.err != nil {
		return &fields{err: f.err}
	}
	return structFields(p)
}

// Dict reads a property dictionary written by builder.Dict.
func (f *fields) Dict() map[string]string {
	d := f.Struct()
	n := d.Int()
	m := map[string]string{}
	for i := int32(0); i < n && d.err == nil; i++ {
		k := d.String()
		m[k] = d.String()
	}
	if d.err != nil && f.err == nil {
		f.err = d.err
	}
	return m
}

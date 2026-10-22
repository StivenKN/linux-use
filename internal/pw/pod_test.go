package pw

import "testing"

func TestPodRoundTrip(t *testing.T) {
	var w builder
	w.Struct(func() {
		w.Int(-3).ID(7).Long(1 << 40).String("abc")
		w.Dict([]string{"node.name", "linux-use"})
		w.Object(objFormat, paramFormat, func() {
			w.Prop(formatVideoFormat).Choice(choiceEnum, typeID, idValue(videoBGRA), idValue(videoBGRA), idValue(videoBGRx))
			w.Prop(formatVideoSize).Choice(choiceNone, typeRectangle, fractionValue(1920, 1080))
		})
	})
	if len(w.bytes())%8 != 0 {
		t.Fatalf("POD is %d bytes, not 8-aligned", len(w.bytes()))
	}
	f := parseStruct(w.bytes())
	if i, id, l, s := f.Int(), f.ID(), f.Long(), f.String(); i != -3 || id != 7 || l != 1<<40 || s != "abc" {
		t.Errorf("scalars = %d %d %d %q", i, id, l, s)
	}
	if d := f.Dict(); d["node.name"] != "linux-use" {
		t.Errorf("dict = %v", d)
	}
	typ, id, props, err := f.Pod().object()
	if err != nil || f.Err() != nil {
		t.Fatal(err, f.Err())
	}
	if typ != objFormat || id != paramFormat {
		t.Errorf("object %#x/%d", typ, id)
	}
	if v, _ := props[formatVideoFormat].uint32Of(typeID); v != videoBGRA {
		t.Errorf("format = %d, want the enum default %d", v, videoBGRA)
	}
	if w, h, _ := props[formatVideoSize].pair(typeRectangle); w != 1920 || h != 1080 {
		t.Errorf("size = %dx%d", w, h)
	}
	if f.Int(); f.Err() == nil {
		t.Error("reading past the end succeeded")
	}
}

func TestReadPodTruncated(t *testing.T) {
	var w builder
	b := w.String("hello").bytes()
	if _, _, err := readPod(b[:10]); err == nil {
		t.Error("truncated POD parsed")
	}
}

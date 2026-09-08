package cursorwire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
)

func pb(n int, payload []byte) []byte {
	b := binary.AppendUvarint(nil, uint64(n<<3|2))
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}
func number(n int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(n<<3)), v)
}
func frame(flags byte, body []byte) []byte {
	b := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(len(body)))
	return append(b, body...)
}
func zipped(b []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Write(b)
	w.Close()
	return out.Bytes()
}
func endMessage(fields ...[]byte) []byte { return pb(1, pb(14, bytes.Join(fields, nil))) }

func TestModelsAndBidiEncodings(t *testing.T) {
	run := pb(1, append(pb(3, pb(1, []byte("fallback"))), pb(9, pb(1, []byte("grok-4.6")))...))
	if got := RunModel(run, false); got != "grok-4.6" {
		t.Fatal(got)
	}
	if got := RunModel([]byte(`{"runRequest":{"modelDetails":{"modelId":"fallback"},"requestedModel":{"modelId":"grok-4.6"}}}`), true); got != "grok-4.6" {
		t.Fatal(got)
	}
	for _, binaryData := range []bool{false, true} {
		body := pb(2, pb(1, []byte("fake-handle")))
		if binaryData {
			body = append(body, pb(4, run)...)
		} else {
			body = append(body, pb(1, []byte(hex.EncodeToString(run)))...)
		}
		if id, model := AppendModel(body, false); id != "fake-handle" || model != "grok-4.6" {
			t.Fatal("initial append not recognized")
		}
		if id, _ := AppendModel(append(body, number(3, 1)...), false); id != "" {
			t.Fatal("noninitial append recognized")
		}
	}
	jsonAppend, _ := json.Marshal(map[string]any{"requestId": map[string]string{"requestId": "fake-handle"}, "dataBinary": run, "appendSeqno": "0"})
	if id, model := AppendModel(jsonAppend, true); id != "fake-handle" || model != "grok-4.6" {
		t.Fatal("JSON append not recognized")
	}
	for _, raw := range [][]byte{nil, {0xff}, append(run, 0xff), pb(1, pb(9, pb(1, []byte("private prompt text"))))} {
		if RunModel(raw, false) != "" {
			t.Fatal("malformed or unsafe model accepted")
		}
	}
}

func TestOptionalFinalUsage(t *testing.T) {
	complete := endMessage(number(1, 1000), number(2, 20), number(3, 700), number(4, 100), number(5, 5))
	got, found := TurnEnd(complete, false)
	if !found || got != (End{Input: 1000, Output: 20, CacheRead: 700, CacheWrite: 100, Reasoning: 5, Complete: true}) {
		t.Fatalf("%+v %v", got, found)
	}
	for _, raw := range [][]byte{
		endMessage(), endMessage(number(1, 1000), number(2, 20), number(3, 0)),
		endMessage(number(1, 100), number(2, 0), number(3, 80), number(4, 80)),
		endMessage(number(1, ^uint64(0)), number(2, 0), number(3, 0), number(4, 0)),
		endMessage(number(1, 100), number(2, 0), number(3, 0), number(4, 0), []byte{0xff}),
	} {
		if end, found := TurnEnd(raw, false); !found || end.Complete {
			t.Fatalf("invalid usage considered complete: %+v %v", end, found)
		}
	}
	if end, found := TurnEnd(endMessage(number(1, 0), number(2, 0), number(3, 0), number(4, 0)), false); !found || !end.Complete {
		t.Fatal("explicit zero is complete")
	}
	for _, input := range []string{`"1000"`, `1000`, `null`, `-1`, `"1.5"`, `"1000000000001"`} {
		raw := []byte(`{"interactionUpdate":{"turnEnded":{"inputTokens":` + input + `,"outputTokens":"20","cacheReadTokens":"700","cacheWriteTokens":"100"}}}`)
		end, found := TurnEnd(raw, true)
		want := input == `"1000"` || input == `1000`
		if !found || end.Complete != want {
			t.Fatalf("input %s: %+v %v", input, end, found)
		}
	}
}

func TestObserveChunkedCompressedAndErrors(t *testing.T) {
	message := endMessage(number(1, 1000), number(2, 20), number(3, 700), number(4, 100))
	for _, compressed := range []bool{false, true} {
		var body []byte
		if compressed {
			body = frame(1, zipped(message))
		} else {
			body = frame(0, message)
		}
		body = append(body, frame(2, []byte(`{"error":{"code":"internal","message":"private response"}}`))...)
		for _, step := range []int{1, 4, 7, len(body)} {
			count, errors := 0, 0
			o := NewObserver("application/connect+proto", "gzip", func(b []byte, j bool) {
				count++
				if !bytes.Equal(b, message) || j {
					t.Fatal("message changed")
				}
			})
			o.EndStreamError = func() { errors++ }
			for off := 0; off < len(body); off += step {
				o.Write(body[off:min(off+step, len(body))])
			}
			o.Close()
			o.Close()
			if count != 1 || errors != 1 {
				t.Fatalf("messages=%d errors=%d", count, errors)
			}
		}
	}
	count := 0
	o := NewObserver("application/proto", "gzip", func(b []byte, _ bool) {
		count++
		if !bytes.Equal(b, message) {
			t.Fatal("unary changed")
		}
	})
	body := zipped(message)
	reader := ObserveReader(io.NopCloser(bytes.NewReader(body)), o)
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, body) || count != 1 {
		t.Fatal("forwarding or unary observation changed")
	}
}

func TestObserverSkipsOversizeUnknownAndTruncated(t *testing.T) {
	valid := frame(0, []byte("valid"))
	body := frame(0, bytes.Repeat([]byte{0x61}, MaxMessage+7))
	body = append(body, frame(4, []byte("unknown flags"))...)
	body = append(body, frame(1, zipped(bytes.Repeat([]byte{0x61}, MaxMessage+1)))...)
	body = append(body, valid...)
	body = append(body, valid[:len(valid)-1]...)
	count := 0
	o := NewObserver("application/connect+proto", "gzip", func(b []byte, _ bool) {
		count++
		if string(b) != "valid" {
			t.Fatal("oversize suffix parsed")
		}
	})
	for off := 0; off < len(body); off += 8191 {
		o.Write(body[off:min(off+8191, len(body))])
	}
	o.Close()
	if count != 1 {
		t.Fatal(count)
	}
}

func FuzzWireNeverPanics(f *testing.F) {
	f.Add([]byte{10, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		RunModel(b, false)
		RunModel(b, true)
		AppendModel(b, false)
		AppendModel(b, true)
		TurnEnd(b, false)
		TurnEnd(b, true)
		o := NewObserver("application/connect+proto", "gzip", func(b []byte, j bool) { TurnEnd(b, j) })
		o.Write(b)
		o.Close()
	})
}

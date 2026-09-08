// Package cursorwire observes only model identifiers and final usage in Cursor's
// Connect protocol. Unknown fields are skipped; payloads never enter diagnostics.
package cursorwire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"regexp"
	"strings"
	"sync"
)

// MaxMessage bounds observation memory, not forwarded request/response sizes.
const MaxMessage = 4 << 20

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)

func ValidModel(model string) bool { return modelPattern.MatchString(model) }

type field struct {
	number int
	wire   uint64
	value  uint64
	bytes  []byte
}

func walk(data []byte, visit func(field)) bool {
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag>>3 == 0 || tag>>3 > (1<<29)-1 {
			return false
		}
		data = data[n:]
		f := field{number: int(tag >> 3), wire: tag & 7}
		switch f.wire {
		case 0:
			f.value, n = binary.Uvarint(data)
			if n <= 0 {
				return false
			}
		case 1:
			n = 8
		case 2:
			length, k := binary.Uvarint(data)
			if k <= 0 || length > uint64(len(data)-k) {
				return false
			}
			data = data[k:]
			n = int(length)
			f.bytes = data[:n]
		case 5:
			n = 4
		default:
			return false
		}
		if n > len(data) {
			return false
		}
		data = data[n:]
		visit(f)
	}
	return true
}

func nested(data []byte, number int) ([]byte, bool) {
	var value []byte
	ok := walk(data, func(f field) {
		if f.number == number && f.wire == 2 {
			value = f.bytes
		}
	})
	return value, ok && value != nil
}

func modelID(data []byte) string {
	id, ok := nested(data, 1)
	if !ok || !ValidModel(string(id)) {
		return ""
	}
	return string(id)
}

// RunModel reads AgentClientMessage.run_request, preserving the requested ID.
func RunModel(data []byte, isJSON bool) string {
	if len(data) > MaxMessage {
		return ""
	}
	if isJSON {
		var message struct {
			RunRequest struct {
				ModelDetails struct {
					ModelID string `json:"modelId"`
				} `json:"modelDetails"`
				RequestedModel *struct {
					ModelID string `json:"modelId"`
				} `json:"requestedModel"`
			} `json:"runRequest"`
		}
		if json.Unmarshal(data, &message) != nil {
			return ""
		}
		id := message.RunRequest.ModelDetails.ModelID
		if message.RunRequest.RequestedModel != nil {
			id = message.RunRequest.RequestedModel.ModelID
		}
		if ValidModel(id) {
			return id
		}
		return ""
	}
	run, ok := nested(data, 1)
	if !ok {
		return ""
	}
	if requested, ok := nested(run, 9); ok {
		return modelID(requested)
	}
	details, _ := nested(run, 3)
	return modelID(details)
}

// RequestID is a transient subscription handle. Callers must never log it.
func RequestID(data []byte, isJSON bool) string {
	if len(data) > MaxMessage {
		return ""
	}
	var id string
	if isJSON {
		var value struct {
			RequestID string `json:"requestId"`
		}
		if json.Unmarshal(data, &value) != nil {
			return ""
		}
		id = value.RequestID
	} else {
		value, ok := nested(data, 1)
		if !ok {
			return ""
		}
		id = string(value)
	}
	if len(id) == 0 || len(id) > 128 {
		return ""
	}
	return id
}

// AppendModel extracts only the initial BidiAppend's correlation ID and model.
func AppendModel(data []byte, isJSON bool) (id, model string) {
	if len(data) > MaxMessage {
		return "", ""
	}
	var raw []byte
	var text string
	var seq uint64
	if isJSON {
		var value struct {
			RequestID  json.RawMessage `json:"requestId"`
			Data       string          `json:"data"`
			DataBinary []byte          `json:"dataBinary"`
			Seq        json.RawMessage `json:"appendSeqno"`
		}
		if json.Unmarshal(data, &value) != nil {
			return "", ""
		}
		if len(value.Seq) > 0 && string(value.Seq) != `"0"` && string(value.Seq) != "0" {
			return "", ""
		}
		id, raw, text = RequestID(value.RequestID, true), value.DataBinary, value.Data
	} else if !walk(data, func(f field) {
		switch {
		case f.number == 1 && f.wire == 2:
			text = string(f.bytes)
		case f.number == 2 && f.wire == 2:
			id = RequestID(f.bytes, false)
		case f.number == 3 && f.wire == 0:
			seq = f.value
		case f.number == 4 && f.wire == 2:
			raw = f.bytes
		}
	}) {
		return "", ""
	}
	if id == "" || seq != 0 {
		return "", ""
	}
	if len(raw) == 0 && text != "" {
		var err error
		raw, err = hex.DecodeString(text)
		if err != nil {
			return "", ""
		}
	}
	model = RunModel(raw, false)
	if model == "" {
		return "", ""
	}
	return id, model
}

// End contains whole-turn counts. Input includes both cache components; reasoning
// is a separate diagnostic breakdown, never added to output for charging.
type End struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	Complete                                        bool
}

const maxTokens = int64(1_000_000_000_000)

// TurnEnd returns found=true even when optional usage fields are absent.
func TurnEnd(data []byte, isJSON bool) (end End, found bool) {
	if len(data) > MaxMessage {
		return end, false
	}
	var values [5]int64
	var present uint8
	valid := true
	if isJSON {
		var message struct {
			InteractionUpdate struct {
				TurnEnded *struct {
					Input     json.RawMessage `json:"inputTokens"`
					Output    json.RawMessage `json:"outputTokens"`
					Read      json.RawMessage `json:"cacheReadTokens"`
					Write     json.RawMessage `json:"cacheWriteTokens"`
					Reasoning json.RawMessage `json:"reasoningTokens"`
				} `json:"turnEnded"`
			} `json:"interactionUpdate"`
		}
		if json.Unmarshal(data, &message) != nil || message.InteractionUpdate.TurnEnded == nil {
			return end, false
		}
		v := message.InteractionUpdate.TurnEnded
		for i, raw := range []json.RawMessage{v.Input, v.Output, v.Read, v.Write, v.Reasoning} {
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			var number json.Number
			if raw[0] == '"' {
				var s string
				if json.Unmarshal(raw, &s) != nil {
					valid = false
					continue
				}
				number = json.Number(s)
			} else {
				number = json.Number(string(raw))
			}
			n, err := number.Int64()
			if err != nil || n < 0 || n > maxTokens {
				valid = false
				continue
			}
			values[i], present = n, present|(1<<i)
		}
	} else {
		update, ok := nested(data, 1)
		if !ok {
			return end, false
		}
		turn, ok := nested(update, 14)
		if !ok {
			return end, false
		}
		valid = walk(turn, func(f field) {
			if f.number < 1 || f.number > 5 {
				return
			}
			if f.wire != 0 || f.value > uint64(maxTokens) {
				valid = false
				return
			}
			values[f.number-1], present = int64(f.value), present|(1<<(f.number-1))
		}) && valid
	}
	end = End{Input: values[0], Output: values[1], CacheRead: values[2], CacheWrite: values[3], Reasoning: values[4]}
	end.Complete = valid && present&15 == 15 && end.CacheRead+end.CacheWrite <= end.Input
	return end, true
}

// Observer handles unary or enveloped Connect messages, including gzip. It never
// changes forwarded bytes; oversize and unsupported messages are simply skipped.
type Observer struct {
	mu                             sync.Mutex
	json, framed, disabled, closed bool
	encoding                       string
	header                         [5]byte
	head                           int
	remaining                      uint32
	flags                          byte
	skip                           bool
	buffer                         []byte
	message                        func([]byte, bool)
	// EndStreamError is set before reading; it receives no upstream error text.
	EndStreamError func()
}

func NewObserver(contentType, encoding string, message func([]byte, bool)) *Observer {
	media, _, _ := mime.ParseMediaType(contentType)
	return &Observer{json: strings.HasSuffix(media, "json"), framed: strings.HasPrefix(media, "application/connect+") || media == "text/event-stream",
		disabled: media != "application/proto" && media != "application/json" && media != "application/connect+proto" && media != "application/connect+json" && media != "text/event-stream",
		encoding: strings.ToLower(encoding), message: message}
}

func (o *Observer) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(data)
	if o.disabled || o.closed {
		return n, nil
	}
	if !o.framed {
		if len(o.buffer)+n > MaxMessage {
			o.disabled = true
			o.buffer = nil
		} else {
			o.buffer = append(o.buffer, data...)
		}
		return n, nil
	}
	for len(data) > 0 {
		if o.head < 5 {
			k := copy(o.header[o.head:], data)
			o.head += k
			data = data[k:]
			if o.head < 5 {
				break
			}
			o.flags = o.header[0]
			o.remaining = binary.BigEndian.Uint32(o.header[1:])
			o.skip = o.remaining > MaxMessage || o.flags & ^byte(3) != 0
		}
		k := len(data)
		if uint64(k) > uint64(o.remaining) {
			k = int(o.remaining)
		}
		if !o.skip {
			o.buffer = append(o.buffer, data[:k]...)
		}
		o.remaining -= uint32(k)
		data = data[k:]
		if o.remaining == 0 {
			if !o.skip {
				o.deliver(o.flags&1 != 0)
			}
			o.buffer = nil
			o.head = 0
		}
	}
	return n, nil
}

func (o *Observer) deliver(compressed bool) {
	if !o.framed && o.encoding != "" && o.encoding != "identity" && o.encoding != "gzip" {
		return
	}
	data := o.buffer
	if compressed {
		if o.encoding != "gzip" {
			return
		}
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		data, err = io.ReadAll(io.LimitReader(reader, MaxMessage+1))
		reader.Close()
		if err != nil || len(data) > MaxMessage {
			return
		}
	}
	if o.framed && o.flags&2 != 0 {
		var metadata struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &metadata) == nil && len(metadata.Error) > 0 && string(metadata.Error) != "null" && o.EndStreamError != nil {
			o.EndStreamError()
		}
		return
	}
	if len(data) > 0 {
		o.message(data, o.json)
	}
}

func (o *Observer) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.closed = true
	if !o.disabled && !o.framed {
		o.deliver(o.encoding == "gzip")
	}
	o.buffer = nil
}

type reader struct {
	io.ReadCloser
	observer *Observer
}

func ObserveReader(body io.ReadCloser, o *Observer) io.ReadCloser { return &reader{body, o} }
func (r *reader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.observer.Write(p[:n])
	if err == io.EOF {
		r.observer.Close()
	}
	return n, err
}
func (r *reader) Close() error { err := r.ReadCloser.Close(); r.observer.Close(); return err }

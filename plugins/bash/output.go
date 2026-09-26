package bash

import "bytes"

// outputTail retains only the bounded suffix needed by both the response and
// spill file. exec.Cmd serializes writes because stdout and stderr share it.
type outputTail struct {
	limit        int
	data         []byte
	start        int
	droppedLines int
	droppedBytes int64
}

func (o *outputTail) Write(data []byte) (int, error) {
	n := len(data)
	if available := o.limit - len(o.data); available > 0 {
		keep := min(available, len(data))
		if needed := len(o.data) + keep; needed > cap(o.data) {
			grown := make([]byte, len(o.data), min(o.limit, max(needed, 2*cap(o.data))))
			copy(grown, o.data)
			o.data = grown
		}
		o.data = append(o.data, data[:keep]...)
		data = data[keep:]
	}
	if len(data) == 0 {
		return n, nil
	}
	if len(data) >= o.limit {
		o.droppedLines += bytes.Count(o.data, []byte{'\n'}) + bytes.Count(data[:len(data)-o.limit], []byte{'\n'})
		o.droppedBytes += int64(len(data))
		copy(o.data, data[len(data)-o.limit:])
		o.start = 0
		return n, nil
	}

	first := min(len(data), o.limit-o.start)
	o.droppedLines += bytes.Count(o.data[o.start:o.start+first], []byte{'\n'})
	o.droppedLines += bytes.Count(o.data[:len(data)-first], []byte{'\n'})
	o.droppedBytes += int64(len(data))
	copy(o.data[o.start:], data[:first])
	copy(o.data, data[first:])
	o.start = (o.start + len(data)) % o.limit
	return n, nil
}

func (o *outputTail) String() string {
	if o.start == 0 {
		return string(o.data)
	}
	return string(o.data[o.start:]) + string(o.data[:o.start])
}

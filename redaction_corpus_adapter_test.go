package nuntius

import "bytes"

// Compatibility exists only in the permanent port of the historical probes.
// Production adapters use the sealed result directly. This wrapper is not the
// differential oracle and has no classifier of its own.
const privateHidden = redactionHidden

type privateEventPrivacy struct {
	input []byte
	state struct{ ambiguous, orphan bool }
}

func (p *privateEventPrivacy) project(wire []byte) ([]byte, bool) {
	line := bytes.Clone(wire)
	added := len(line) == 0 || line[len(line)-1] != '\n'
	if added {
		line = append(line, '\n')
	}
	p.input = append(p.input, line...)
	result := approveEvents(p.input, false)
	last := result.events[len(result.events)-1]
	p.state.ambiguous, p.state.orphan = last.hidden, last.hidden
	if added {
		return bytes.TrimSuffix(last.wire, []byte("\n")), last.hidden
	}
	return last.wire, last.hidden
}

func privateEventProjection(wire []byte, _ privateEventPrivacy) [][]byte {
	var out [][]byte
	for _, event := range approveEvents(wire, false).events {
		out = append(out, bytes.TrimSuffix(event.wire, []byte("\n")))
	}
	if len(wire) > 0 && wire[len(wire)-1] == '\n' {
		out = append(out, nil)
	}
	return out
}

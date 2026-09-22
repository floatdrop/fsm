package fsm

import (
	"fmt"
	"slices"
	"strings"
)

// States returns every state mentioned in the definition, in declaration
// order.
func (m *Machine[S]) States() []S { return slices.Clone(m.states) }

// Edges returns every registered transition, in declaration order.
func (m *Machine[S]) Edges() []Edge[S] { return slices.Clone(m.edges) }

// Terminals returns the states with no outgoing transition.
//
// A machine's terminal set is worth asserting in a test: an unintended
// terminal state is a state something can get stuck in.
func (m *Machine[S]) Terminals() []S {
	out := []S{}
	for _, s := range m.states {
		if !slices.ContainsFunc(m.edges, func(e Edge[S]) bool { return e.From == s }) {
			out = append(out, s)
		}
	}
	return out
}

// Unreachable returns the states that cannot be reached from initial by any
// sequence of transitions, ignoring guards. initial itself is always
// considered reachable.
func (m *Machine[S]) Unreachable(initial S) []S {
	reached := map[S]bool{initial: true}
	for changed := true; changed; {
		changed = false
		for _, e := range m.edges {
			if reached[e.From] && !reached[e.To] {
				reached[e.To] = true
				changed = true
			}
		}
	}

	out := []S{}
	for _, s := range m.states {
		if !reached[s] {
			out = append(out, s)
		}
	}
	return out
}

// DOT renders the machine as a Graphviz digraph.
//
// Output is deterministic: states and edges are emitted in declaration order,
// never in map order, so the result can be committed and diffed.
func (m *Machine[S]) DOT() string {
	var b strings.Builder
	fmt.Fprintf(&b, "digraph \"%s\" {\n", dotEscape(m.name))
	b.WriteString("\trankdir=LR;\n")

	terminals := m.Terminals()
	for _, s := range m.states {
		shape := "box"
		if slices.Contains(terminals, s) {
			shape = "doublecircle"
		}
		fmt.Fprintf(&b, "\t\"%s\" [shape=%s];\n", dotEscape(fmt.Sprint(s)), shape)
	}

	for _, e := range m.edges {
		// The guard is appended after escaping so that the \n stays a
		// Graphviz line break rather than becoming a literal backslash-n.
		label := dotEscape(e.Event)
		if e.Guard != "" {
			label += `\n[` + dotEscape(e.Guard) + `]`
		}
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\" [label=\"%s\"];\n",
			dotEscape(fmt.Sprint(e.From)), dotEscape(fmt.Sprint(e.To)), label)
	}

	b.WriteString("}\n")
	return b.String()
}

// dotEscape makes s safe inside a double-quoted Graphviz string.
func dotEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

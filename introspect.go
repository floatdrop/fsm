package fsm

import (
	"fmt"
	"slices"
	"strings"
)

// States returns every state mentioned in the definition, in declaration
// order.
func (m *Machine[S]) States() []S { return slices.Clone(m.states) }

// Edges returns every registered transition, in declaration order. Rows a
// group transition expanded to carry the group's name in [Edge.Group].
func (m *Machine[S]) Edges() []Edge[S] { return slices.Clone(m.edges) }

// Groups returns every declared group, in declaration order.
func (m *Machine[S]) Groups() []Group[S] { return slices.Clone(m.groups) }

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

// DOT renders the machine as a Graphviz digraph. A [Group] is drawn as a
// cluster, and a transition every member inherited is drawn once from the
// cluster boundary rather than once per member.
//
// Output is deterministic: states, edges and groups are emitted in declaration
// order, never in map order, so the result can be committed and diffed.
func (m *Machine[S]) DOT() string {
	var b strings.Builder
	fmt.Fprintf(&b, "digraph \"%s\" {\n", dotEscape(m.name))
	b.WriteString("\trankdir=LR;\n")
	if len(m.groups) > 0 {
		b.WriteString("\tcompound=true;\n") // lets an edge stop at a cluster boundary
	}

	terminals := m.Terminals()
	node := func(indent string, s S) {
		shape := "box"
		if slices.Contains(terminals, s) {
			shape = "doublecircle"
		}
		fmt.Fprintf(&b, "%s\"%s\" [shape=%s];\n", indent, dotEscape(fmt.Sprint(s)), shape)
	}

	// Graphviz puts a node in one cluster, so a state in overlapping groups is
	// drawn in the first that claims it.
	owner := make(map[S]string, len(m.states))
	for _, g := range m.groups {
		fmt.Fprintf(&b, "\tsubgraph \"cluster_%s\" {\n", dotEscape(g.name))
		fmt.Fprintf(&b, "\t\tlabel=\"%s\";\n", dotEscape(g.name))
		b.WriteString("\t\tstyle=rounded;\n")
		for _, s := range g.members {
			if _, taken := owner[s]; taken {
				continue
			}
			owner[s] = g.name
			node("\t\t", s)
		}
		b.WriteString("\t}\n")
	}
	for _, s := range m.states {
		if _, taken := owner[s]; !taken {
			node("\t", s)
		}
	}

	// A cluster can only be the tail of an edge if it really holds every one
	// of its members; an overlapping group that lost some to an earlier
	// cluster cannot, and Graphviz would drop the ltail with a warning.
	whole := make(map[string]bool, len(m.groups))
	for _, g := range m.groups {
		whole[g.name] = !slices.ContainsFunc(g.members, func(s S) bool { return owner[s] != g.name })
	}

	// Rows are counted per group transition, identified by the group and the
	// trigger itself — two events can share a name, so counting by name would
	// merge two transitions and collapse an arrow that covers neither.
	type boundary struct {
		group   string
		trigger *eventDef
	}
	of := func(e Edge[S]) boundary { return boundary{group: e.Group, trigger: e.trigger} }
	rows := make(map[boundary]int)
	for _, e := range m.edges {
		if e.Group != "" {
			rows[of(e)]++
		}
	}

	byName := make(map[string]Group[S], len(m.groups))
	for _, g := range m.groups {
		byName[g.name] = g
	}

	drawn := make(map[boundary]bool)
	for _, e := range m.edges {
		// The guard is appended after escaping so that the \n stays a
		// Graphviz line break rather than becoming a literal backslash-n.
		label := dotEscape(e.Event)
		if e.Guard != "" {
			label += `\n[` + dotEscape(e.Guard) + `]`
		}

		var attrs string
		if e.Group != "" && collapsible(e, byName[e.Group], rows[of(e)], whole[e.Group]) {
			k := of(e)
			if drawn[k] {
				continue
			}
			drawn[k] = true
			attrs = fmt.Sprintf(`, ltail="cluster_%s"`, dotEscape(e.Group))
		}
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\" [label=\"%s\"%s];\n",
			dotEscape(fmt.Sprint(e.From)), dotEscape(fmt.Sprint(e.To)), label, attrs)
	}

	b.WriteString("}\n")
	return b.String()
}

// collapsible reports whether e may be drawn once from its cluster's boundary
// instead of once per member. Every case that says no would otherwise lose a
// row from the diagram, because Graphviz refuses the ltail and the other rows
// have already been skipped:
//
//   - a member overrode the event, so the arrow would claim to cover it;
//   - the group does not hold all its members in its own cluster;
//   - the target is inside the group, which makes the arrow a self-loop out
//     of its own cluster.
func collapsible[S comparable](e Edge[S], g Group[S], rows int, whole bool) bool {
	return whole && rows == len(g.members) && !g.Has(e.To)
}

// dotEscape makes s safe inside a double-quoted Graphviz string.
func dotEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

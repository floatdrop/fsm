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

// Events returns the distinct names of the events some transition reacts
// to, in declaration order. Names are for display; two events sharing one
// are listed once.
func (m *Machine[S]) Events() []string {
	seen := make(map[string]bool, len(m.edges))
	out := []string{}
	for _, e := range m.edges {
		if name := e.Event(); !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Initial returns the state declared with [Initial], and false when none was.
func (m *Machine[S]) Initial() (S, bool) { return m.initial, m.hasInitial }

// Terminals returns the states with no outgoing transition. An internal
// transition, declared with [OnStep.Stay], is not a way out.
//
// A machine's terminal set is worth asserting in a test: an unintended
// terminal state is a state something can get stuck in.
func (m *Machine[S]) Terminals() []S {
	out := []S{}
	for _, s := range m.states {
		if !slices.ContainsFunc(m.edges, func(e Edge[S]) bool { return e.From == s && !e.Internal }) {
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
// cluster, a transition every member inherited is drawn once from the
// cluster boundary rather than once per member, and the [Initial] state, if
// declared, is pointed at from a dot.
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
	// The start marker's node id must not be a state's name.
	start := "__start"
	if m.hasInitial {
		for slices.ContainsFunc(m.states, func(s S) bool { return fmt.Sprint(s) == start }) {
			start += "_"
		}
		fmt.Fprintf(&b, "\t\"%s\" [shape=point];\n", start)
	}

	terminals := m.Terminals()
	node := func(indent string, s S) {
		shape := "box"
		if slices.Contains(terminals, s) {
			shape = "doublecircle"
		}
		fmt.Fprintf(&b, "%s\"%s\" [shape=%s];\n", indent, dotEscape(fmt.Sprint(s)), shape)
	}

	// Graphviz puts a node in one cluster, and nests clusters, so groups
	// that are disjoint or contain one another are drawn as they are, a
	// group inside the smallest that contains it and a state inside the
	// smallest group that holds it. A group that partly overlaps an earlier
	// drawn one cannot be: it gets no cluster, and a comment says so.
	drawn := m.drawableGroups()
	parent := make(map[string]string, len(drawn))
	owner := make(map[S]string, len(m.states))
	for i, g := range drawn {
		if h, ok := innermost(drawn, g.members, i); ok {
			parent[g.name] = h
		}
	}
	for _, s := range m.states {
		if h, ok := innermost(drawn, []S{s}, -1); ok {
			owner[s] = h
		}
	}
	var cluster func(g Group[S], indent string)
	cluster = func(g Group[S], indent string) {
		fmt.Fprintf(&b, "%ssubgraph \"cluster_%s\" {\n", indent, dotEscape(g.name))
		fmt.Fprintf(&b, "%s\tlabel=\"%s\";\n", indent, dotEscape(g.name))
		fmt.Fprintf(&b, "%s\tstyle=rounded;\n", indent)
		for _, h := range drawn {
			if parent[h.name] == g.name {
				cluster(h, indent+"\t")
			}
		}
		for _, s := range g.members {
			if owner[s] == g.name {
				node(indent+"\t", s)
			}
		}
		fmt.Fprintf(&b, "%s}\n", indent)
	}
	for _, g := range drawn {
		if _, inner := parent[g.name]; !inner {
			cluster(g, "\t")
		}
	}
	for _, g := range m.groups {
		if !slices.ContainsFunc(drawn, func(h Group[S]) bool { return h.name == g.name }) {
			fmt.Fprintf(&b, "\t// group %s overlaps another without containing it, so it is not drawn as a cluster\n",
				commentReplacer.Replace(g.name))
		}
	}
	for _, s := range m.states {
		if _, grouped := owner[s]; !grouped {
			node("\t", s)
		}
	}

	// A cluster can only be the tail of an edge if it really holds every one
	// of its members, which a drawn group does, nested clusters included.
	whole := make(map[string]bool, len(drawn))
	for _, g := range drawn {
		whole[g.name] = true
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

	if m.hasInitial {
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\";\n", start, dotEscape(fmt.Sprint(m.initial)))
	}
	collapsed := make(map[boundary]bool)
	for _, e := range m.edges {
		// The guard is appended after escaping so that the \n stays a
		// Graphviz line break rather than becoming a literal backslash-n.
		label := dotEscape(e.Event())
		if e.Guard != "" {
			label += `\n[` + dotEscape(e.Guard) + `]`
		}

		var attrs string
		if e.Internal {
			attrs = ", style=dashed" // handled in the state, which it does not leave
		}
		if e.Group != "" && collapsible(e, byName[e.Group], rows[of(e)], whole[e.Group]) {
			k := of(e)
			if collapsed[k] {
				continue
			}
			collapsed[k] = true
			attrs = fmt.Sprintf(`, ltail="cluster_%s"`, dotEscape(e.Group))
		}
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\" [label=\"%s\"%s];\n",
			dotEscape(fmt.Sprint(e.From)), dotEscape(fmt.Sprint(e.To)), label, attrs)
	}

	b.WriteString("}\n")
	return b.String()
}

// drawableGroups returns the groups that can be drawn as clusters, in
// declaration order: each one disjoint from, inside, or around every earlier
// one drawn. One that partly overlaps an earlier one is left out.
func (m *Machine[S]) drawableGroups() []Group[S] {
	var out []Group[S]
	for _, g := range m.groups {
		if !slices.ContainsFunc(out, func(h Group[S]) bool { return crosses(g, h) }) {
			out = append(out, g)
		}
	}
	return out
}

// crosses reports whether g and h share a member while neither holds all of
// the other's, which no nesting of clusters can draw.
func crosses[S comparable](g, h Group[S]) bool {
	shared := slices.ContainsFunc(g.members, h.Has)
	return shared && !subset(g.members, h) && !subset(h.members, g)
}

func subset[S comparable](ss []S, g Group[S]) bool {
	return !slices.ContainsFunc(ss, func(s S) bool { return !g.Has(s) })
}

// innermost names the smallest group of drawn holding all of ss, other
// than drawn[self] (self is -1 for a state), and reports whether there is
// one. Of two groups with the same members the later is inside the earlier,
// so a twin holds drawn[self] only if declared before it.
func innermost[S comparable](drawn []Group[S], ss []S, self int) (string, bool) {
	best, found := -1, false
	for i, h := range drawn {
		if i == self || !subset(ss, h) {
			continue
		}
		if self >= 0 && len(h.members) == len(ss) && self < i {
			continue // a twin declared after self is inside it, not around it
		}
		if !found || len(h.members) <= len(drawn[best].members) {
			best, found = i, true
		}
	}
	if !found {
		return "", false
	}
	return drawn[best].name, true
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

// commentReplacer keeps a name on its line in a // comment.
var commentReplacer = strings.NewReplacer("\n", " ", "\r", " ")

// dotReplacer makes a string safe inside double quotes in Graphviz.
var dotReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func dotEscape(s string) string { return dotReplacer.Replace(s) }

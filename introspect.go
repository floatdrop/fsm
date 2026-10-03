package fsm

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
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

	l := m.layout()
	l.nest(func(g Group[S], depth int) {
		indent := strings.Repeat("\t", depth)
		fmt.Fprintf(&b, "%ssubgraph \"cluster_%s\" {\n", indent, dotEscape(g.name))
		fmt.Fprintf(&b, "%s\tlabel=\"%s\";\n", indent, dotEscape(g.name))
		fmt.Fprintf(&b, "%s\tstyle=rounded;\n", indent)
	}, func(s S, depth int) {
		node(strings.Repeat("\t", depth), s)
	}, func(depth int) {
		fmt.Fprintf(&b, "%s}\n", strings.Repeat("\t", depth))
	})
	for _, g := range l.unboxed {
		fmt.Fprintf(&b, "\t// group %s overlaps another without containing it, so it is not drawn as a cluster\n",
			commentReplacer.Replace(g.name))
	}
	for _, s := range m.states {
		if _, grouped := l.owner[s]; !grouped {
			node("\t", s)
		}
	}

	if m.hasInitial {
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\";\n", start, dotEscape(fmt.Sprint(m.initial)))
	}
	for i, e := range m.edges {
		if l.skip[i] {
			continue
		}
		// The separator is added after escaping so that the \n stays a
		// Graphviz line break rather than becoming a literal backslash-n.
		label := e.label(dotEscape, `\n`)

		var attrs string
		if e.Internal {
			attrs = ", style=dashed" // handled in the state, which it does not leave
		}
		if l.tail[i] != "" {
			attrs = fmt.Sprintf(`, ltail="cluster_%s"`, dotEscape(l.tail[i]))
		}
		fmt.Fprintf(&b, "\t\"%s\" -> \"%s\" [label=\"%s\"%s];\n",
			dotEscape(fmt.Sprint(e.From)), dotEscape(fmt.Sprint(e.To)), label, attrs)
	}

	b.WriteString("}\n")
	return b.String()
}

// Mermaid renders the machine as a Mermaid state diagram, for a mermaid
// code block in Markdown, which GitHub draws. It draws what [Machine.DOT]
// draws, in Mermaid's terms: a [Group] is a composite state, nested and
// left out exactly as DOT's clusters are, with a transition every member
// inherited drawn once from it under the same conditions; an internal
// transition is a line inside its state rather than a loop; the [Initial]
// state is entered from [*], and a terminal state leads to [*].
//
// Output is deterministic, as DOT's is, so it can be committed and diffed.
func (m *Machine[S]) Mermaid() string {
	ids := make(map[S]string, len(m.states))
	for i, s := range m.states {
		ids[s] = "s" + strconv.Itoa(i)
	}
	gids := make(map[string]string, len(m.groups))
	for i, g := range m.groups {
		gids[g.name] = "g" + strconv.Itoa(i)
	}
	indent := func(depth int) string { return strings.Repeat("    ", depth) }

	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %s\n---\n", strconv.Quote(m.name)) // a YAML double-quoted string
	b.WriteString("stateDiagram-v2\n    direction LR\n")
	l := m.layout()
	l.nest(func(g Group[S], depth int) {
		fmt.Fprintf(&b, "%sstate \"%s\" as %s {\n", indent(depth), mermaidEscape(g.name), gids[g.name])
	}, func(s S, depth int) {
		fmt.Fprintf(&b, "%s%s\n", indent(depth), ids[s])
	}, func(depth int) {
		fmt.Fprintf(&b, "%s}\n", indent(depth))
	})
	// Mermaid reads a %%{ directive even inside a comment, so the name is
	// escaped here too.
	for _, g := range l.unboxed {
		fmt.Fprintf(&b, "    %%%% group %s overlaps another without containing it, so it is not drawn as a composite state\n",
			mermaidEscape(g.name))
	}

	// A state's first description line is its name, and each internal
	// transition adds one below it.
	inside := make(map[S][]Edge[S])
	for _, e := range m.edges {
		if e.Internal {
			inside[e.From] = append(inside[e.From], e)
		}
	}
	for _, s := range m.states {
		fmt.Fprintf(&b, "    %s : %s\n", ids[s], mermaidEscape(fmt.Sprint(s)))
		for _, e := range inside[s] {
			fmt.Fprintf(&b, "    %s : %s\n", ids[s], e.label(mermaidEscape, " "))
		}
	}

	if m.hasInitial {
		fmt.Fprintf(&b, "    [*] --> %s\n", ids[m.initial])
	}
	for i, e := range m.edges {
		if e.Internal || l.skip[i] {
			continue
		}
		from := ids[e.From]
		if l.tail[i] != "" {
			from = gids[l.tail[i]]
		}
		fmt.Fprintf(&b, "    %s --> %s : %s\n", from, ids[e.To], e.label(mermaidEscape, "<br>"))
	}
	for _, s := range m.Terminals() {
		fmt.Fprintf(&b, "    %s --> [*]\n", ids[s])
	}
	return b.String()
}

// label is e's event and its guard in brackets, each escaped, joined by
// sep, which is not.
func (e Edge[S]) label(escape func(string) string, sep string) string {
	label := escape(e.Event())
	if e.Guard != "" {
		label += sep + "[" + escape(e.Guard) + "]"
	}
	return label
}

// layout is how a diagram draws the groups: which it boxes, how the boxes
// nest and which holds each state, and which edges it draws once from a
// box's boundary instead of once per member.
type layout[S comparable] struct {
	drawn   []Group[S]
	unboxed []Group[S]        // the groups left out of drawn, in declaration order
	parent  map[string]string // a drawn group's innermost enclosing one
	owner   map[S]string      // a state's innermost drawn group
	tail    []string          // per edge: the group it is drawn from, or ""
	skip    []bool            // per edge: drawn already, from its group
}

// layout works out how m's groups are drawn. A box holds a state, or
// another box, only once, and boxes nest, so groups that are disjoint or
// contain one another are drawn as they are, a group inside the smallest
// that contains it and a state inside the smallest group that holds it. A
// group that partly overlaps an earlier drawn one cannot be: it gets no box.
func (m *Machine[S]) layout() layout[S] {
	l := layout[S]{
		drawn:  m.drawableGroups(),
		parent: make(map[string]string),
		owner:  make(map[S]string, len(m.states)),
		tail:   make([]string, len(m.edges)),
		skip:   make([]bool, len(m.edges)),
	}
	boxed := make(map[string]bool, len(l.drawn))
	for i, g := range l.drawn {
		boxed[g.name] = true
		if h, ok := innermost(l.drawn, g.members, i); ok {
			l.parent[g.name] = h
		}
	}
	for _, g := range m.groups {
		if !boxed[g.name] {
			l.unboxed = append(l.unboxed, g)
		}
	}
	for _, s := range m.states {
		if h, ok := innermost(l.drawn, []S{s}, -1); ok {
			l.owner[s] = h
		}
	}

	// Rows are counted per group transition, identified by the group and the
	// trigger itself — two events can share a name, so counting by name would
	// merge two transitions and collapse an arrow that covers neither.
	type boundary struct {
		group   string
		trigger *eventDef
	}
	rows := make(map[boundary]int)
	for _, e := range m.edges {
		if e.Group != "" {
			rows[boundary{e.Group, e.trigger}]++
		}
	}
	byName := make(map[string]Group[S], len(m.groups))
	for _, g := range m.groups {
		byName[g.name] = g
	}
	drawnFrom := make(map[boundary]bool)
	for i, e := range m.edges {
		k := boundary{e.Group, e.trigger}
		if e.Group == "" || !collapsible(e, byName[e.Group], rows[k], boxed[e.Group]) {
			continue
		}
		l.tail[i], l.skip[i] = e.Group, drawnFrom[k]
		drawnFrom[k] = true
	}
	return l
}

// nest walks the boxes depth first, in declaration order: open and close
// bracket each, and state is called for each state a box holds directly,
// after the boxes inside it. An outermost box is at depth 1.
func (l *layout[S]) nest(open func(g Group[S], depth int), state func(s S, depth int), closed func(depth int)) {
	var walk func(g Group[S], depth int)
	walk = func(g Group[S], depth int) {
		open(g, depth)
		for _, h := range l.drawn {
			if l.parent[h.name] == g.name {
				walk(h, depth+1)
			}
		}
		for _, s := range g.members {
			if l.owner[s] == g.name {
				state(s, depth+1)
			}
		}
		closed(depth)
	}
	for _, g := range l.drawn {
		if _, inner := l.parent[g.name]; !inner {
			walk(g, 1)
		}
	}
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

// mermaidReplacer keeps a string on its line, and writes as entity codes
// the characters Mermaid would read as syntax or markup.
var mermaidReplacer = strings.NewReplacer("#", "#35;", `"`, "#34;", ":", "#58;", ";", "#59;",
	"%", "#37;", "&", "#38;", "<", "#60;", ">", "#62;", "\n", " ", "\r", " ")

// mermaidEscape makes s safe as a Mermaid label. An empty one is a
// zero-width space, since Mermaid shows an id in place of an empty label.
func mermaidEscape(s string) string { return cmp.Or(mermaidReplacer.Replace(s), "#8203;") }

// dotReplacer makes a string safe inside double quotes in Graphviz.
var dotReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func dotEscape(s string) string { return dotReplacer.Replace(s) }

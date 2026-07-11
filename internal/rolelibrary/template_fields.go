package rolelibrary

import (
	"sort"
	"text/template"
	"text/template/parse"
)

// templateFields returns the distinct top-level field names a parsed
// template references via `{{.field}}` (and dotted forms like
// `{{.field.sub}}`, whose first identifier is `field`). Used by the
// doctor check to enforce that a prompt body's splice points are all
// declared in promptParams.
//
// It walks the template's parse tree collecting *parse.FieldNode
// identifiers. Action pipelines, `if`/`range`/`with` branches, and
// nested command arguments are all traversed so a splice point buried
// in a conditional is still caught.
func templateFields(t *template.Template) []string {
	set := map[string]bool{}
	if t == nil || t.Tree == nil {
		return nil
	}
	walkNode(t.Root, set)
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func walkNode(n parse.Node, set map[string]bool) {
	if n == nil {
		return
	}
	switch node := n.(type) {
	case *parse.ListNode:
		if node == nil {
			return
		}
		for _, c := range node.Nodes {
			walkNode(c, set)
		}
	case *parse.ActionNode:
		walkPipe(node.Pipe, set)
	case *parse.IfNode:
		walkBranch(&node.BranchNode, set)
	case *parse.RangeNode:
		walkBranch(&node.BranchNode, set)
	case *parse.WithNode:
		walkBranch(&node.BranchNode, set)
	case *parse.TemplateNode:
		walkPipe(node.Pipe, set)
	}
}

func walkBranch(b *parse.BranchNode, set map[string]bool) {
	if b == nil {
		return
	}
	walkPipe(b.Pipe, set)
	walkNode(b.List, set)
	walkNode(b.ElseList, set)
}

func walkPipe(p *parse.PipeNode, set map[string]bool) {
	if p == nil {
		return
	}
	for _, cmd := range p.Cmds {
		for _, arg := range cmd.Args {
			if fn, ok := arg.(*parse.FieldNode); ok && len(fn.Ident) > 0 {
				set[fn.Ident[0]] = true
			}
		}
	}
}

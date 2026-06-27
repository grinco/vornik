package ui

// Collection reconcile — the save primitive for repeating sub-objects
// (Swarm roles[], later Workflow steps{}). Where applyYAMLSequenceElement
// Patches edits ONE existing element's fields, this reconciles the WHOLE
// sequence to a desired ordered set: surviving elements are reused (so
// their comments and unedited keys survive) and patched in place, absent
// elements are dropped, new ids are appended as fresh mappings, and the
// final order is the submitted order. Identity is the idKey value.

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// collectionItem is one desired element of a reconciled sequence: the
// idKey value that identifies it plus the field patches (rooted at the
// element) to apply. For an existing element the patches update it in
// place; for a new id they populate a fresh mapping. Patches should
// include the idKey field so a new element carries its identity.
type collectionItem struct {
	ID      string
	Patches []yamlPatch
}

// mappingItem is one desired entry of a reconciled mapping: the map key
// that identifies it plus the field patches (rooted at the value mapping)
// to apply. Unlike collectionItem the key is NOT patched into the value —
// it IS the map key (rename = drop old key + add new).
type mappingItem struct {
	Key     string
	Patches []yamlPatch
}

// reconcileYAMLMapping rewrites the top-level mappingKey mapping (e.g.
// steps{}) to match items: surviving keys reuse their existing key + value
// nodes (comments preserved) and are patched in place, absent keys are
// dropped, new keys are appended, and the order is the submitted order
// (cosmetic for a map). A missing mappingKey is synthesised. Same
// yaml.Node preservation guarantee as applyYAMLPatches.
func reconcileYAMLMapping(content []byte, mappingKey string, items []mappingItem) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("yaml has unexpected top-level shape")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml top-level must be a mapping, got kind %d", doc.Kind)
	}

	_, mapNode := findMappingChild(doc, mappingKey)
	if mapNode == nil {
		mapNode = &yaml.Node{Kind: yaml.MappingNode}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: mappingKey},
			mapNode,
		)
	}
	if mapNode.Kind != yaml.MappingNode {
		if mapNode.Kind == yaml.ScalarNode && (mapNode.Tag == "!!null" || mapNode.Value == "") {
			mapNode.Kind = yaml.MappingNode
			mapNode.Tag = ""
			mapNode.Value = ""
		} else {
			return nil, fmt.Errorf("key %q is not a mapping (kind=%d)", mappingKey, mapNode.Kind)
		}
	}

	// Index existing key/value node pairs so survivors keep comments.
	type kv struct{ key, val *yaml.Node }
	existing := make(map[string]kv, len(mapNode.Content)/2)
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		existing[mapNode.Content[i].Value] = kv{mapNode.Content[i], mapNode.Content[i+1]}
	}

	newContent := make([]*yaml.Node, 0, len(items)*2)
	for _, it := range items {
		pair, ok := existing[it.Key]
		if !ok {
			pair = kv{
				key: &yaml.Node{Kind: yaml.ScalarNode, Value: it.Key},
				val: &yaml.Node{Kind: yaml.MappingNode},
			}
		}
		for _, p := range it.Patches {
			if err := applyOnePatch(pair.val, p); err != nil {
				return nil, fmt.Errorf("item %q: %w", it.Key, err)
			}
		}
		newContent = append(newContent, pair.key, pair.val)
	}
	mapNode.Content = newContent

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("encode reconciled yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return out.Bytes(), nil
}

// reconcileYAMLSequence rewrites the top-level sequenceKey sequence to
// match items (order + membership), reusing existing element nodes by
// idKey so their comments survive. A missing sequenceKey is synthesised.
// Comments on surviving elements, sibling top-level keys, and unrelated
// content are preserved — same yaml.Node guarantee as applyYAMLPatches.
func reconcileYAMLSequence(content []byte, sequenceKey, idKey string, items []collectionItem) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("yaml has unexpected top-level shape")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml top-level must be a mapping, got kind %d", doc.Kind)
	}

	_, seqNode := findMappingChild(doc, sequenceKey)
	if seqNode == nil {
		// Synthesise an empty sequence under sequenceKey.
		seqNode = &yaml.Node{Kind: yaml.SequenceNode}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: sequenceKey},
			seqNode,
		)
	}
	if seqNode.Kind != yaml.SequenceNode {
		// An empty/absent value may decode as a null scalar — coerce it.
		if seqNode.Kind == yaml.ScalarNode && (seqNode.Tag == "!!null" || seqNode.Value == "") {
			seqNode.Kind = yaml.SequenceNode
			seqNode.Tag = ""
			seqNode.Value = ""
		} else {
			return nil, fmt.Errorf("key %q is not a sequence (kind=%d)", sequenceKey, seqNode.Kind)
		}
	}

	// Index existing elements by idKey value so we can reuse their nodes.
	existing := make(map[string]*yaml.Node, len(seqNode.Content))
	for _, elem := range seqNode.Content {
		if elem.Kind != yaml.MappingNode {
			continue
		}
		if _, idVal := findMappingChild(elem, idKey); idVal != nil {
			existing[idVal.Value] = elem
		}
	}

	newContent := make([]*yaml.Node, 0, len(items))
	for _, it := range items {
		elem := existing[it.ID]
		if elem == nil {
			elem = &yaml.Node{Kind: yaml.MappingNode}
		}
		for _, p := range it.Patches {
			if err := applyOnePatch(elem, p); err != nil {
				return nil, fmt.Errorf("item %q: %w", it.ID, err)
			}
		}
		newContent = append(newContent, elem)
	}
	seqNode.Content = newContent

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, fmt.Errorf("encode reconciled yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return out.Bytes(), nil
}

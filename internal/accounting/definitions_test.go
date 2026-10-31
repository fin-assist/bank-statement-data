package accounting

import (
	"slices"
	"testing"
)

func TestUnitMetricDefinitionsAreIndependentAndComplete(t *testing.T) {
	definitions := Definitions()
	if CatalogVersion() != 3 || len(definitions) != 15 {
		t.Fatal("initial catalog identity changed without updating the baseline")
	}
	seen := make(map[string]bool)
	for _, definition := range definitions {
		if definition.ID == "" || definition.Version < 1 || seen[definition.ID] {
			t.Fatal("invalid or duplicated definition", definition)
		}
		seen[definition.ID] = true
		if got, ok := DefinitionFor(definition.ID); !ok || got != definition {
			t.Fatal("definition lookup changed", definition)
		}
	}
	definitions[0].Version = 42
	definitions[1].ID = "foreign"
	if got, ok := DefinitionFor("expense_categories"); !ok || got.Version != 1 {
		t.Fatal("caller mutated the catalog")
	}
	if got, ok := DefinitionFor("unknown"); ok || got != (Definition{}) {
		t.Fatal("unknown metric inherited a version")
	}
}

func TestUnitRegistryFingerprintChangesWithDefinitionsAndSharedRules(t *testing.T) {
	baseDefinitions := []Definition{{"a", 1}, {"b", 3}}
	baseFeatures := []Feature{{"gate", 2, 0}, {"other", 0, 3}}
	rules := []int64{72, 60, 15}
	words := []string{"комиссия", "процент"}
	fingerprint := func(v int32, d []Definition, f []Feature, r []int64, w []string) string {
		return catalogFingerprint(v, d, f, r, w)
	}
	base := fingerprint(1, baseDefinitions, baseFeatures, rules, words)
	first, second := CatalogFingerprint(), CatalogFingerprint()
	if len(base) != 64 || first == "" || first != second {
		t.Fatal("unstable fingerprint")
	}
	reversedDefinitions, reversedFeatures, reversedWords := slices.Clone(baseDefinitions), slices.Clone(baseFeatures), slices.Clone(words)
	slices.Reverse(reversedDefinitions)
	slices.Reverse(reversedFeatures)
	slices.Reverse(reversedWords)
	if fingerprint(1, reversedDefinitions, reversedFeatures, rules, reversedWords) != base || reversedDefinitions[0].ID != "b" || reversedFeatures[0].Name != "other" || reversedWords[0] != "процент" {
		t.Fatal("ordering changes the fingerprint or caller input was mutated")
	}
	variants := []string{
		fingerprint(2, baseDefinitions, baseFeatures, rules, words),
		fingerprint(1, append(slices.Clone(baseDefinitions), Definition{"new.metric", 1}), baseFeatures, rules, words),
		fingerprint(1, []Definition{{"a", 2}, {"b", 3}}, baseFeatures, rules, words),
		fingerprint(1, baseDefinitions[:1], baseFeatures, rules, words),
		fingerprint(1, baseDefinitions, []Feature{{"gate", 3, 0}, {"other", 0, 3}}, rules, words),
		fingerprint(1, baseDefinitions, baseFeatures, []int64{72, 60, 16}, words),
		fingerprint(1, baseDefinitions, baseFeatures, rules, append(slices.Clone(words), "новая")),
	}
	for _, variant := range variants {
		if variant == base {
			t.Fatal("added/changed definition, threshold or rule did not change fingerprint")
		}
	}
}

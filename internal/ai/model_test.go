package ai

import "testing"

func TestBuiltinModelsReturnsCatalog(t *testing.T) {
	models := BuiltinModels()

	totalModels := 0
	for _, providerModels := range modelRegistry {
		totalModels += len(providerModels)
	}
	if len(models) != totalModels {
		t.Fatalf("BuiltinModels len = %d, want %d", len(models), totalModels)
	}
	for i := 1; i < len(models); i++ {
		prev := models[i-1]
		curr := models[i]
		if prev.Provider > curr.Provider || (prev.Provider == curr.Provider && prev.ID > curr.ID) {
			t.Fatalf("BuiltinModels order[%d:%d] = %s/%s before %s/%s",
				i-1,
				i,
				prev.Provider,
				prev.ID,
				curr.Provider,
				curr.ID,
			)
		}
	}

	for _, model := range models {
		catalogModel, ok := GetModel(model.Provider, model.ID)
		if !ok {
			t.Fatalf("BuiltinModels returned unknown model %q (provider %q)", model.ID, model.Provider)
		}
		if model.Provider != catalogModel.Provider || model.API != catalogModel.API {
			t.Fatalf("model %q = %+v, want catalog %+v", model.ID, model, catalogModel)
		}
	}

	models[0].Input[0] = "mutated"
	again, ok := GetModel(models[0].Provider, models[0].ID)
	if !ok {
		t.Fatalf("GetModel %q failed", models[0].ID)
	}
	if again.Input[0] == "mutated" {
		t.Fatal("BuiltinModels exposed catalog slice storage")
	}
}

func TestGetModelReturnsDefensiveCopy(t *testing.T) {
	provider := "__copy_provider__"
	id := "__copy_model__"
	level := "max"
	maxTokensField := "max_tokens"
	supportsStore := true
	allowFallbacks := true
	dataCollection := "deny"

	modelRegistry[provider] = map[string]Model{
		id: {
			ID:       id,
			Name:     id,
			Provider: provider,
			Input:    []InputCapability{InputText},
			Headers:  map[string]string{"X-Test": "original"},
			ThinkingLevelMap: map[string]*string{
				"xhigh": &level,
			},
			Compat: &Compat{
				SupportsStore:  &supportsStore,
				MaxTokensField: &maxTokensField,
				OpenRouterRouting: &OpenRouterRouting{
					AllowFallbacks: &allowFallbacks,
					DataCollection: &dataCollection,
					Order:          []string{"first"},
					MaxPrice:       map[string]any{"prompt": 1.0},
				},
			},
		},
	}
	t.Cleanup(func() {
		delete(modelRegistry, provider)
	})

	got, ok := GetModel(provider, id)
	if !ok {
		t.Fatal("GetModel = false")
	}
	got.Input[0] = InputImage
	got.Headers["X-Test"] = "mutated"
	*got.ThinkingLevelMap["xhigh"] = "mutated"
	*got.Compat.SupportsStore = false
	*got.Compat.MaxTokensField = "mutated"
	*got.Compat.OpenRouterRouting.AllowFallbacks = false
	*got.Compat.OpenRouterRouting.DataCollection = "mutated"
	got.Compat.OpenRouterRouting.Order[0] = "mutated"
	got.Compat.OpenRouterRouting.MaxPrice["prompt"] = 9.0

	again, ok := GetModel(provider, id)
	if !ok {
		t.Fatal("GetModel again = false")
	}
	if again.Input[0] != InputText ||
		again.Headers["X-Test"] != "original" ||
		*again.ThinkingLevelMap["xhigh"] != "max" ||
		!*again.Compat.SupportsStore ||
		*again.Compat.MaxTokensField != "max_tokens" ||
		!*again.Compat.OpenRouterRouting.AllowFallbacks ||
		*again.Compat.OpenRouterRouting.DataCollection != "deny" ||
		again.Compat.OpenRouterRouting.Order[0] != "first" ||
		again.Compat.OpenRouterRouting.MaxPrice["prompt"] != 1.0 {
		t.Fatalf("GetModel exposed catalog storage: %+v", again)
	}
}

func TestGetModelRequiresProviderForDuplicateIDs(t *testing.T) {
	id := "__duplicate_model__"
	modelRegistry["__duplicate_a__"] = map[string]Model{
		id: {ID: id, Name: "A", Provider: "__duplicate_a__"},
	}
	modelRegistry["__duplicate_b__"] = map[string]Model{
		id: {ID: id, Name: "B", Provider: "__duplicate_b__"},
	}
	t.Cleanup(func() {
		delete(modelRegistry, "__duplicate_a__")
		delete(modelRegistry, "__duplicate_b__")
	})

	if _, ok := GetModel("", id); ok {
		t.Fatal("bare duplicate GetModel = true, want false")
	}
	got, ok := GetModel("__duplicate_b__", id)
	if !ok {
		t.Fatal("provider-qualified GetModel = false")
	}
	if got.Provider != "__duplicate_b__" {
		t.Fatalf("provider-qualified model provider = %q", got.Provider)
	}
}

package catalog

// DeepMergeCatalog merges catalog layers. Objects recurse; arrays, primitives, and null replace whole.
func DeepMergeCatalog(bundled, user map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range bundled {
		out[k] = CloneJSONValue(v)
	}
	for k, userValue := range user {
		bundledMap, bundledOK := out[k].(map[string]any)
		userMap, userOK := userValue.(map[string]any)
		if bundledOK && userOK {
			out[k] = DeepMergeCatalog(bundledMap, userMap)
			continue
		}
		out[k] = CloneJSONValue(userValue)
	}
	return out
}

// ShallowMergeBody merges request body sidecars top-level only. Later layers win.
func ShallowMergeBody(layers ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, layer := range layers {
		for k, v := range layer {
			out[k] = CloneJSONValue(v)
		}
	}
	return out
}

// CloneJSONValue deep-copies one decoded JSON value tree: objects, slices,
// and the immutable leaf kinds string, bool, nil, float64 and json.Number.
func CloneJSONValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, value := range typed {
			out[k] = CloneJSONValue(value)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, value := range typed {
			out[i] = CloneJSONValue(value)
		}
		return out
	default:
		return typed
	}
}

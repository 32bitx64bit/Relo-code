package prompt

import "sync"

// artTaskProperty is the task flag that offers an agent the art tools. It is part of the Primary's schema only while
// the art tools are switched on, so a session that never makes art pays nothing for it.
var artTaskProperty = map[string]any{
	"type":        "boolean",
	"description": "True when the task makes image assets such as sprites, icons or textures. Relo then gives the agent its sprite tool and a model that can see. Name the reference images, the target size and the file to write in the description, and ask for a preview of the result next to the reference in acceptance. For a list of items, make one art task per item, not one task for all of them: every picture costs the user real money against a session budget, so each task should stop when its sprite passes. Make the first item first and have the others depend on it, so they can match its finished sprite and the brief it wrote.",
}

var (
	artSchemaOnce sync.Once
	artSchema     map[string]any
)

// PrimarySchemaFor returns the Primary's reply schema. With art, each task may carry the art flag.
func PrimarySchemaFor(art bool) map[string]any {
	if !art {
		return PrimarySchema
	}
	artSchemaOnce.Do(func() {
		props := map[string]any{}
		for k, v := range taskSchema["properties"].(map[string]any) {
			props[k] = v
		}
		props["art"] = artTaskProperty
		task := map[string]any{}
		for k, v := range taskSchema {
			task[k] = v
		}
		task["properties"] = props
		top := map[string]any{}
		for k, v := range PrimarySchema {
			top[k] = v
		}
		topProps := map[string]any{}
		for k, v := range PrimarySchema["properties"].(map[string]any) {
			topProps[k] = v
		}
		topProps["tasks"] = map[string]any{"type": "array", "items": task}
		top["properties"] = topProps
		artSchema = top
	})
	return artSchema
}

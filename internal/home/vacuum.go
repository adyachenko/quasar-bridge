package home

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"strings"
)

// CleanArea is a Home Assistant area that a vacuum can clean on its own:
// the vacuum's entity options map the area to segments of its map.
type CleanArea struct {
	ID      string   `json:"area_id"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// cleanAreas lists the areas mapped to vacuum segments in the entity registry
// options (vacuum.area_mapping), which vacuum.clean_area resolves on the Home
// Assistant side. A vacuum without a mapping gets no areas.
func (c *Controller) cleanAreas(ctx context.Context, id string) ([]CleanArea, error) {
	raw, err := c.Tools.Call(ctx, "ha_call_service", map[string]any{"ws_command": "config/entity_registry/get", "data": map[string]any{"entity_id": id}})
	if err != nil {
		return nil, err
	}
	var entry struct {
		Result struct {
			Options struct {
				Vacuum struct {
					AreaMapping map[string][]string `json:"area_mapping"`
				} `json:"vacuum"`
			} `json:"options"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, errors.New("invalid vacuum registry entry")
	}
	mapping := entry.Result.Options.Vacuum.AreaMapping
	if len(mapping) == 0 {
		return nil, nil
	}
	raw, err = c.Tools.Call(ctx, "ha_call_service", map[string]any{"ws_command": "config/area_registry/list"})
	if err != nil {
		return nil, err
	}
	var registry struct {
		Result []CleanArea `json:"result"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil, errors.New("invalid area registry")
	}
	var areas []CleanArea
	for _, area := range registry.Result {
		if len(mapping[area.ID]) > 0 {
			area.Aliases = slices.DeleteFunc(area.Aliases, func(alias string) bool { return strings.TrimSpace(alias) == "" })
			areas = append(areas, area)
		}
	}
	return areas, nil
}

// withCleanAreas attaches clean areas to an exposed vacuum. A registry read
// failure leaves the vacuum without areas: whole-home commands keep working
// and validation rejects any room cleaning.
func (c *Controller) withCleanAreas(ctx context.Context, e Entity) Entity {
	if !strings.HasPrefix(e.ID, "vacuum.") {
		return e
	}
	areas, err := c.cleanAreas(ctx, e.ID)
	if err != nil {
		slog.Warn("vacuum_clean_areas_failed", "entity_id", e.ID, "error", err)
		return e
	}
	e.CleanAreas = areas
	return e
}

// cleanAreaHint explains room cleaning only when a vacuum can do it.
func cleanAreaHint(entities []Entity) string {
	for _, e := range entities {
		if len(e.CleanAreas) > 0 {
			return ` Уборка отдельных комнат («пропылесось в детской», «убери кухню и коридор») — vacuum.clean_area с "data": {"cleaning_area_id": [area_id, ...]}, только area_id из clean_areas этого пылесоса, все комнаты одним действием. «Пропылесось всё, кроме детской» — все clean_areas пылесоса без названных. Уборка всего дома без исключений — vacuum.start. Если названной комнаты нет в clean_areas, спроси, без действий.`
		}
	}
	return ""
}

// checkCleanAreas lets vacuum.clean_area target only the vacuum's mapped areas;
// Home Assistant would start the mapped ones before failing on the rest.
func checkCleanAreas(a Action, e Entity) error {
	if a.Domain != "vacuum" || a.Service != "clean_area" {
		return nil
	}
	var ids []string
	switch value := a.Data["cleaning_area_id"].(type) {
	case string:
		ids = []string{value}
	case []any:
		for _, item := range value {
			id, ok := item.(string)
			if !ok {
				return errors.New("home vacuum area not allowed")
			}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return errors.New("home vacuum area not allowed")
	}
	for _, id := range ids {
		if !slices.ContainsFunc(e.CleanAreas, func(area CleanArea) bool { return area.ID == id }) {
			return errors.New("home vacuum area not allowed")
		}
	}
	return nil
}

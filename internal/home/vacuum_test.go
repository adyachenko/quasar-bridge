package home

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"

	"quasarbridge/internal/llm"
)

type vacuumFixture struct {
	mapping     map[string][]string
	registryErr bool
	calls       []map[string]any
}

func (f *vacuumFixture) Call(_ context.Context, name string, args map[string]any) (jsontext.Value, error) {
	var value any
	switch name {
	case "ha_get_skill_guide":
		value = map[string]any{"success": true, "content": "fixture"}
	case "ha_search":
		value = map[string]any{"entities": []any{map[string]any{"entity_id": "vacuum.robot", "friendly_name": "Пылесос", "state": "docked"}}, "has_more": false}
	case "ha_get_entity_exposure":
		value = map[string]any{"success": true, "exposed_to": map[string]any{"conversation": true}}
	case "ha_get_state":
		value = map[string]any{"data": map[string]any{"entity_id": "vacuum.robot", "state": "cleaning"}}
	case "ha_call_service":
		switch args["ws_command"] {
		case "config/entity_registry/get":
			if f.registryErr {
				return nil, errors.New("registry unavailable")
			}
			value = map[string]any{"success": true, "result": map[string]any{"entity_id": "vacuum.robot", "options": map[string]any{"vacuum": map[string]any{"area_mapping": f.mapping}}}}
		case "config/area_registry/list":
			value = map[string]any{"success": true, "result": []any{
				map[string]any{"area_id": "detskaia", "name": "Детская", "aliases": []any{}},
				map[string]any{"area_id": "koridor", "name": "Коридор", "aliases": []any{}},
				map[string]any{"area_id": "komnata_maksa", "name": "Комната Макса", "aliases": []any{"у Макса", ""}},
				map[string]any{"area_id": "kukhnia", "name": "Кухня", "aliases": []any{}},
			}}
		case nil:
			f.calls = append(f.calls, args)
			value = map[string]any{"success": true}
		default:
			panic("unexpected ws command")
		}
	default:
		panic("unexpected tool")
	}
	data, _ := json.Marshal(value)
	return data, nil
}

func newVacuumFixture() *vacuumFixture {
	return &vacuumFixture{mapping: map[string][]string{"detskaia": {"1_2"}, "koridor": {"1_3"}, "komnata_maksa": {"1_1"}, "kukhnia": {}}}
}

func vacuumPlan(data string) plannerFunc {
	return func(context.Context, []llm.Message, bool) (string, error) {
		return `{"answer":"","actions":[{"domain":"vacuum","service":"clean_area","entity_id":"vacuum.robot","data":` + data + `}]}`, nil
	}
}

func TestVacuumCleansMappedAreasInOneCall(t *testing.T) {
	f := newVacuumFixture()
	var system, user string
	c := Controller{Tools: f, Domains: []string{"vacuum"}, Planner: plannerFunc(func(ctx context.Context, m []llm.Message, s bool) (string, error) {
		system, user = m[0].Content, m[1].Content
		return vacuumPlan(`{"cleaning_area_id":["koridor","komnata_maksa"]}`)(ctx, m, s)
	})}
	answer, err := c.Handle(context.Background(), "Пропылесось всё, кроме детской")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("service calls = %d, want one cleaning for all rooms", len(f.calls))
	}
	call := f.calls[0]
	areas, _ := call["data"].(map[string]any)["cleaning_area_id"].([]any)
	if call["domain"] != "vacuum" || call["service"] != "clean_area" || call["entity_id"] != "vacuum.robot" || len(areas) != 2 || areas[0] != "koridor" || areas[1] != "komnata_maksa" {
		t.Fatalf("call = %v", call)
	}
	if !strings.Contains(system, "vacuum.clean_area") || !strings.Contains(system, "кроме детской") {
		t.Fatalf("prompt lacks room cleaning rules: %s", system)
	}
	// Only mapped areas reach the planner, with names and aliases to match speech;
	// the unmapped kitchen and the empty alias stay out.
	if !strings.Contains(user, `"clean_areas":[{"area_id":"detskaia","name":"Детская"}`) || !strings.Contains(user, `"aliases":["у Макса"]`) || strings.Contains(user, "kukhnia") {
		t.Fatalf("inventory = %s", user)
	}
	if answer != "Команда отправлена: Пылесос." {
		t.Fatalf("answer = %q", answer)
	}
}

func TestVacuumCleanAreaAcceptsSingleArea(t *testing.T) {
	f := newVacuumFixture()
	c := Controller{Tools: f, Domains: []string{"vacuum"}, Planner: vacuumPlan(`{"cleaning_area_id":"detskaia"}`)}
	if _, err := c.Handle(context.Background(), "Пропылесось в детской"); err != nil || len(f.calls) != 1 {
		t.Fatalf("err = %v, calls = %d", err, len(f.calls))
	}
}

func TestVacuumCleanAreaRejectedBeforeAnyCall(t *testing.T) {
	for name, data := range map[string]string{
		"unmapped_area":       `{"cleaning_area_id":["koridor","kukhnia"]}`,
		"unknown_area":        `{"cleaning_area_id":"garage"}`,
		"empty_list":          `{"cleaning_area_id":[]}`,
		"no_areas":            `{}`,
		"not_a_string":        `{"cleaning_area_id":[1]}`,
		"segment_not_area":    `{"cleaning_area_id":["1_2"]}`,
		"extra_parameter":     `{"cleaning_area_id":["koridor"],"segments":["1_2"]}`,
		"area_name_not_id":    `{"cleaning_area_id":["Коридор"]}`,
		"number_instead_list": `{"cleaning_area_id":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newVacuumFixture()
			c := Controller{Tools: f, Domains: []string{"vacuum"}, Planner: vacuumPlan(data)}
			if _, err := c.Handle(context.Background(), "Пропылесось коридор"); err == nil || len(f.calls) != 0 {
				t.Fatalf("err = %v, calls = %d", err, len(f.calls))
			}
		})
	}
}

func TestVacuumWithoutAreasCannotCleanRooms(t *testing.T) {
	for name, f := range map[string]*vacuumFixture{
		"no_mapping":     {},
		"registry_error": {registryErr: true, mapping: map[string][]string{"koridor": {"1_3"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var system, user string
			c := Controller{Tools: f, Domains: []string{"vacuum"}, Planner: plannerFunc(func(ctx context.Context, m []llm.Message, s bool) (string, error) {
				system, user = m[0].Content, m[1].Content
				return vacuumPlan(`{"cleaning_area_id":["koridor"]}`)(ctx, m, s)
			})}
			if _, err := c.Handle(context.Background(), "Пропылесось коридор"); err == nil || len(f.calls) != 0 {
				t.Fatalf("err = %v, calls = %d", err, len(f.calls))
			}
			if strings.Contains(system, "cleaning_area_id") || strings.Contains(user, "clean_areas") || !strings.Contains(user, "vacuum.robot") {
				t.Fatalf("system = %s\nuser = %s", system, user)
			}
		})
	}
}

func TestCleaningRequestsSelectVacuum(t *testing.T) {
	for _, text := range []string{"Пропылесось в детской", "Запусти уборку в коридоре", "Убери в комнате Макса"} {
		if got := controlDomains(text); !slices.Contains(got, "vacuum") {
			t.Fatalf("%q -> %v", text, got)
		}
	}
}

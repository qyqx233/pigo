// Scenes: prompts an administrator prepares for one kind of task — "查某地区
// 药品的医保自付比例" — each with the tools it needs and the model it runs on
// by default. A scene is used two ways:
//
//   - a scene session: created from the scene, the session's system prompt
//     carries the scene's prompt and its tool set is the scene's; the session
//     keeps a copy of the scene, so later edits do not change it;
//   - a scene command: "/slug question" in any session expands, for that one
//     turn, into the scene's prompt followed by the question, and the scene's
//     tools join the turn.
//
// Scenes live in the settings document, like the price table. The data a scene
// works from is not in the prompt: it is a tool (tools.yaml, scope: scene) the
// prompt tells the model to call.
//
// See spec/preset-scenes.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/smallnest/pigo/internal/agentcore"
)

// scene is one scene as the administrator defines it.
type scene struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Icon        string    `json:"icon,omitempty"`
	Description string    `json:"description,omitempty"`
	Prompt      string    `json:"prompt"`
	Examples    []string  `json:"examples,omitempty"`
	Tools       []string  `json:"tools,omitempty"`
	Model       string    `json:"model,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Thinking    string    `json:"thinking,omitempty"`
	Disabled    bool      `json:"disabled,omitempty"`
	Order       int       `json:"order,omitempty"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// sceneSnapshot is the copy of a scene a session keeps.
type sceneSnapshot struct {
	Slug   string    `json:"slug"`
	Name   string    `json:"name"`
	Icon   string    `json:"icon,omitempty"`
	Prompt string    `json:"prompt"`
	Tools  []string  `json:"tools,omitempty"`
	At     time.Time `json:"at"`
}

func (sc scene) snapshot() *sceneSnapshot {
	return &sceneSnapshot{
		Slug:   sc.Slug,
		Name:   sc.Name,
		Icon:   sc.Icon,
		Prompt: sc.Prompt,
		Tools:  append([]string(nil), sc.Tools...),
		At:     time.Now().UTC(),
	}
}

// sceneSnapshotOf is the copy a new session keeps; nil for no scene.
func sceneSnapshotOf(sc *scene) *sceneSnapshot {
	if sc == nil {
		return nil
	}
	return sc.snapshot()
}

const (
	maxScenePrompt   = 64 << 10
	maxSceneExamples = 5
)

var sceneSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// reservedSlug reports whether a slug is taken by a built-in command.
func reservedSlug(slug string) bool {
	for _, cmd := range webCommands() {
		if cmd.Name == slug {
			return true
		}
	}
	return false
}

// validate normalizes a submitted scene. knownTool says whether a tool name
// exists; nil skips that check.
func (sc scene) validate(knownTool func(string) bool) (scene, error) {
	sc.Slug = strings.ToLower(strings.TrimSpace(sc.Slug))
	sc.Name = strings.TrimSpace(sc.Name)
	sc.Icon = strings.TrimSpace(sc.Icon)
	sc.Description = strings.TrimSpace(sc.Description)
	sc.Prompt = strings.TrimSpace(sc.Prompt)
	sc.Model = strings.TrimSpace(sc.Model)
	sc.Provider = strings.ToLower(strings.TrimSpace(sc.Provider))
	sc.Thinking = strings.TrimSpace(sc.Thinking)
	switch {
	case !sceneSlugPattern.MatchString(sc.Slug):
		return sc, errors.New("命令名须为 2～32 位小写字母、数字或 -，以字母开头")
	case reservedSlug(sc.Slug):
		return sc, fmt.Errorf("/%s 是内置命令，换一个命令名", sc.Slug)
	case sc.Name == "":
		return sc, errors.New("名称不能为空")
	case sc.Prompt == "":
		return sc, errors.New("提示词不能为空")
	case len(sc.Prompt) > maxScenePrompt:
		return sc, fmt.Errorf("提示词超过 %d KB", maxScenePrompt>>10)
	case len([]rune(sc.Icon)) > 4:
		return sc, errors.New("图标最多 4 个字符")
	case sc.Thinking != "" && !validThinking(sc.Thinking):
		return sc, fmt.Errorf("推理强度 %q 无效", sc.Thinking)
	case sc.Provider != "" && sc.Model == "":
		return sc, errors.New("指定了服务商就要指定模型")
	}
	var examples []string
	for _, e := range sc.Examples {
		if e = strings.TrimSpace(e); e != "" {
			examples = append(examples, e)
		}
	}
	if len(examples) > maxSceneExamples {
		return sc, fmt.Errorf("示例问法最多 %d 条", maxSceneExamples)
	}
	sc.Examples = examples
	seen := map[string]bool{}
	var tools []string
	for _, t := range sc.Tools {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if knownTool != nil && !knownTool(t) {
			return sc, fmt.Errorf("没有名为 %s 的工具", t)
		}
		seen[t] = true
		tools = append(tools, t)
	}
	sc.Tools = tools
	return sc, nil
}

// --- the store ------------------------------------------------------------------

// scenes returns every scene, in display order.
func (s *settingsStore) scenes() []scene {
	out := append([]scene(nil), s.get().Scenes...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// scene returns one scene by slug.
func (s *settingsStore) scene(slug string) (scene, bool) {
	for _, sc := range s.get().Scenes {
		if sc.Slug == slug {
			return sc, true
		}
	}
	return scene{}, false
}

var (
	errSceneExists   = errors.New("已有同名命令的场景")
	errSceneNotFound = errors.New("场景不存在")
)

// putScene stores a scene under its slug. from is the slug it is saved over:
// empty to create, another slug to rename.
func (s *settingsStore) putScene(from string, next scene) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]scene(nil), s.settings.Scenes...)
	at := -1
	for i, sc := range s.settings.Scenes {
		if sc.Slug == next.Slug && sc.Slug != from {
			return errSceneExists
		}
		if from != "" && sc.Slug == from {
			at = i
		}
	}
	switch {
	case at >= 0:
		s.settings.Scenes[at] = next
	case from != "":
		return errSceneNotFound
	default:
		s.settings.Scenes = append(s.settings.Scenes, next)
	}
	if err := s.saveLocked(); err != nil {
		s.settings.Scenes = previous
		return err
	}
	return nil
}

// removeScene deletes a scene, reporting whether it existed.
func (s *settingsStore) removeScene(slug string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]scene, 0, len(s.settings.Scenes))
	for _, sc := range s.settings.Scenes {
		if sc.Slug != slug {
			kept = append(kept, sc)
		}
	}
	if len(kept) == len(s.settings.Scenes) {
		return false, nil
	}
	previous := s.settings.Scenes
	s.settings.Scenes = kept
	if err := s.saveLocked(); err != nil {
		s.settings.Scenes = previous
		return false, err
	}
	return true, nil
}

// --- tools --------------------------------------------------------------------

// sceneToolInfo is one tool a scene can list.
type sceneToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Builtin: one of the server's own tools; Scene: a tools.yaml tool with
	// scope: scene, which only scenes get.
	Builtin bool `json:"builtin,omitempty"`
	Scene   bool `json:"scene,omitempty"`
}

// sceneToolChoices lists the tools a scene can name: the built-ins, then the
// extensions.
func (s *apiServer) sceneToolChoices() []sceneToolInfo {
	var out []sceneToolInfo
	for _, name := range hostToolNames {
		if _, replaced := s.exts.tool(name); !replaced {
			out = append(out, sceneToolInfo{Name: name, Builtin: true})
		}
	}
	if s.exts != nil {
		for _, t := range s.exts.tools {
			out = append(out, sceneToolInfo{Name: t.Name, Description: firstLine(strings.TrimSpace(t.Description)), Scene: t.Scope == "scene"})
		}
	}
	return out
}

func (s *apiServer) knownTool(name string) bool {
	for _, t := range s.sceneToolChoices() {
		if t.Name == name {
			return true
		}
	}
	return false
}

// keepTools keeps the tools whose names are listed.
func keepTools(tools []agentcore.AgentTool, names []string) []agentcore.AgentTool {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []agentcore.AgentTool
	for _, t := range tools {
		if want[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// missingTools returns the tools of extra not already in tools.
func missingTools(tools, extra []agentcore.AgentTool) []agentcore.AgentTool {
	have := map[string]bool{}
	for _, t := range tools {
		have[t.Name()] = true
	}
	var out []agentcore.AgentTool
	for _, t := range extra {
		if !have[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

type sceneToolsKey struct{}

// withSceneTools marks a turn as a scene command's, bringing those tools.
func withSceneTools(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, sceneToolsKey{}, names)
}

func sceneToolsFrom(ctx context.Context) []string {
	names, _ := ctx.Value(sceneToolsKey{}).([]string)
	return names
}

// --- prompts ------------------------------------------------------------------

// sceneSystemPrompt is what a scene session appends to the system prompt.
func sceneSystemPrompt(sc *sceneSnapshot) string {
	return "\n\n## 当前场景：" + sc.Name + "\n\n" + sc.Prompt + "\n"
}

// A scene command's message: the scene's prompt in a marked block, then the
// question. The marker lets history show the command the user typed.
const (
	sceneOpen  = "<scene slug=\""
	sceneClose = "</scene>"
)

func sceneCommandMessage(sc scene, question string) string {
	return fmt.Sprintf("%s%s\" name=%q>\n以下是本轮任务的场景说明，请按它完成用户的问题。\n\n%s\n%s\n\n用户问题：%s",
		sceneOpen, sc.Slug, sc.Name, sc.Prompt, sceneClose, question)
}

var sceneHeader = regexp.MustCompile(`^<scene slug="([a-z0-9-]+)" name="((?:[^"\\]|\\.)*)">\n`)

// parseSceneCommand undoes sceneCommandMessage.
func parseSceneCommand(text string) (slug, name, question string, ok bool) {
	m := sceneHeader.FindStringSubmatch(text)
	if m == nil {
		return "", "", "", false
	}
	end := strings.LastIndex(text, sceneClose+"\n\n用户问题：")
	if end < 0 {
		return "", "", "", false
	}
	name = m[2]
	if unquoted, err := strconv.Unquote(`"` + m[2] + `"`); err == nil {
		name = unquoted
	}
	return m[1], name, text[end+len(sceneClose+"\n\n用户问题："):], true
}

// sceneCommand resolves "/slug question" to an enabled scene.
func (s *apiServer) sceneCommand(input string) (sc scene, question string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return scene{}, "", false
	}
	name, rest := trimmed[1:], ""
	if i := strings.IndexFunc(name, unicode.IsSpace); i >= 0 {
		name, rest = name[:i], name[i:]
	}
	sc, found := s.settings.scene(strings.ToLower(name))
	if !found || sc.Disabled {
		return scene{}, "", false
	}
	return sc, strings.TrimSpace(rest), true
}

// sceneUsage is the reply to a bare "/slug".
func sceneUsage(sc scene) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**（/%s）", sc.Name, sc.Slug)
	if sc.Description != "" {
		b.WriteString("：" + sc.Description)
	}
	b.WriteString("\n\n用法：`/" + sc.Slug + " 你的问题`")
	if len(sc.Examples) > 0 {
		b.WriteString("\n\n示例：")
		for _, e := range sc.Examples {
			b.WriteString("\n- `/" + sc.Slug + " " + e + "`")
		}
	}
	return b.String()
}

// sceneHelp lists the enabled scenes for /help.
func (s *apiServer) sceneHelp() string {
	var b strings.Builder
	for _, sc := range s.settings.scenes() {
		if sc.Disabled {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n\nscenes:")
		}
		fmt.Fprintf(&b, "\n  /%s - %s", sc.Slug, sc.Name)
		if sc.Description != "" {
			b.WriteString("：" + sc.Description)
		}
	}
	return asMarkdownLines(b.String())
}

// sceneCommands lists the enabled scenes as slash commands.
func (s *apiServer) sceneCommands() []slashCommand {
	var out []slashCommand
	for _, sc := range s.settings.scenes() {
		if sc.Disabled {
			continue
		}
		desc := sc.Name
		if sc.Description != "" {
			desc += "：" + sc.Description
		}
		out = append(out, slashCommand{Name: sc.Slug, Description: desc, ArgumentHint: "<问题>", Source: "scene", Available: true})
	}
	return out
}

// --- HTTP -----------------------------------------------------------------------

// sceneView is a scene as the API returns it.
type sceneView struct {
	scene
	// MissingTools are listed tools the tools config no longer has.
	MissingTools []string `json:"missingTools,omitempty"`
}

func (s *apiServer) sceneViews(all bool) []sceneView {
	out := []sceneView{}
	for _, sc := range s.settings.scenes() {
		if sc.Disabled && !all {
			continue
		}
		view := sceneView{scene: sc}
		for _, t := range sc.Tools {
			if !s.knownTool(t) {
				view.MissingTools = append(view.MissingTools, t)
			}
		}
		out = append(out, view)
	}
	return out
}

// handleListScenes is every user's list: the enabled scenes, prompts included.
func (s *apiServer) handleListScenes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"scenes": s.sceneViews(false), "tools": s.sceneToolChoices()})
}

func (s *apiServer) handleAdminScenes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"scenes": s.sceneViews(true), "tools": s.sceneToolChoices()})
}

// handlePutScene creates (path slug "new") or updates a scene; a body slug
// other than the path's renames it.
func (s *apiServer) handlePutScene(w http.ResponseWriter, r *http.Request) {
	var request scene
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate(s.knownTool)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from := r.PathValue("slug")
	if from == "new" {
		from = ""
	}
	next.UpdatedBy = principalForRequest(r).Username
	next.UpdatedAt = time.Now().UTC()
	if err := s.settings.putScene(from, next); err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errSceneExists):
			status = http.StatusConflict
		case errors.Is(err, errSceneNotFound):
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	action := "put scene"
	if from != "" && from != next.Slug {
		action = "rename scene " + from + " →"
	}
	s.logAdminAction(r, action, next.Slug)
	writeJSON(w, http.StatusOK, map[string]any{"scenes": s.sceneViews(true), "tools": s.sceneToolChoices()})
}

func (s *apiServer) handleDeleteScene(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	removed, err := s.settings.removeScene(slug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, errSceneNotFound.Error())
		return
	}
	s.logAdminAction(r, "delete scene", slug)
	writeJSON(w, http.StatusOK, map[string]any{"scenes": s.sceneViews(true), "tools": s.sceneToolChoices()})
}

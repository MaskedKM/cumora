// agent 包 avatar/image 面 —— cli.ts cmdAvatar(show/set/regen)+
// setAgentAvatarFromUrl + router.ts generateAndPersistAvatar(确定性视觉
// 签名 + 性别分类 + image API + CH_STATUS 广播)+ cmdImage generate。
package avatar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	agent "github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/obs"
)

// Domain:域子包接收器——嵌入 agent.Service(内核),方法体与拆包前逐字
// 对齐(#140 刀法)。
type Domain struct {
	*agent.Service
}

// 从 router.ts VISUAL_DIMENSIONS 程序化提取(2026-08-27)—— 请勿手改。
var visualAge = []string{
	"21",
	"22",
	"22",
	"23",
	"23",
	"24",
	"24",
	"25",
	"25",
	"26",
	"26",
	"27",
	"28",
	"29",
	"31",
	"32",
}
var visualPresentationFeminine = []string{
	"feminine, softly pretty face with large clear eyes and a small refined nose",
	"feminine, soft heart-shaped face with full pink lips and bright doe eyes",
	"feminine, delicate youthful features with rounded soft cheeks",
	"feminine, doll-like proportions with large expressive eyes and a small chin",
	"feminine, gently pretty face with high cheekbones and a graceful neck",
	"feminine, sweet feminine features with full soft lips and clear bright eyes",
	"feminine, classically pretty youthful face, soft jaw and big warm eyes",
}
var visualPresentationMasculine = []string{
	"masculine, striking refined features with deep clear eyes",
	"masculine, soft handsome face with a strong nose and full lashes",
	"masculine, sharp jaw and high cheekbones, a face with quiet intensity",
	"masculine, refined gentle features with a clear thoughtful gaze",
	"masculine, model-grade proportions with a defined brow",
	"masculine, slim refined face with expressive dark eyes",
	"masculine, soft features with full lips and a kind direct gaze",
}
var visualPresentationAndrogynous = []string{
	"gentle androgynous beauty, soft refined features with a warm clear gaze",
	"softly androgynous, delicate features with deep expressive eyes",
	"softly androgynous, balanced symmetrical features and a kind direct gaze",
	"softly androgynous, refined youthful face with full lashes and a small nose",
	"softly androgynous, pretty refined features with a gentle calm expression",
}
var visualSkin = []string{
	"fair porcelain with a faint pink undertone",
	"fair porcelain with a faint pink undertone",
	"cool ivory",
	"cool ivory",
	"warm cream",
	"warm cream",
	"soft beige",
	"soft beige",
	"golden olive",
	"warm tan",
	"sun-warmed honey",
	"rich olive",
}
var visualHairColor = []string{
	"raven black with a blue undertone",
	"soft espresso brown",
	"warm chestnut with copper highlights",
	"deep auburn",
	"honey blonde",
	"icy platinum",
	"cool ash brown",
	"fiery copper red",
	"jet black with one bleached blonde streak in front",
	"mossy dark green dyed tint",
	"warm caramel blonde",
	"inky black with bluntly chopped fringe",
	"soft pastel-pink ends fading into natural brown",
}
var visualHairStyleFeminine = []string{
	"long soft waves falling over one shoulder",
	"a chin-length soft bob with a gentle inward curl",
	"shoulder-length and softly tousled, parted in the middle",
	"long, almost-waist-length and gathered loosely over the shoulder",
	"a single low ponytail with face-framing strands",
	"a wide halo of natural curls, framing the face softly",
	"long box braids gathered into a low feminine bun",
	"wavy hair tucked behind one ear, falling past the shoulder",
	"a soft half-up topknot with the rest falling in waves",
	"long straight hair with a soft curtain fringe",
	"a sweet bob with side-swept fringe just brushing the chin",
	"a low chignon with delicate face-framing wisps",
}
var visualHairStyleMasculine = []string{
	"soft tousled hair brushed casually forward",
	"an undercut with a long textured top swept to one side",
	"tight coils cropped close to the scalp, sharp hairline",
	"a clean tapered fade with a coiled crown",
	"medium-length hair tucked loosely behind the ears",
	"short waves with natural volume on top",
	"a soft fringe sweeping across the forehead",
	"a chin-length mullet, soft and modern",
	"shoulder-length wavy hair, free and loose",
	"a sharp shape-up with refined natural texture on top",
}
var visualHairStyleAndrogynous = []string{
	"shoulder-length and tucked loosely behind the ears",
	"a soft chin-length bob with a center part",
	"medium-length tousled hair with a gentle curtain fringe",
	"shoulder-length wavy hair, free and loose",
	"a soft mid-length cut brushed casually back",
	"collarbone-length straight hair with a blunt soft fringe",
}
var visualSignaturePool = []string{
	"a small beauty mark just under the left eye",
	"a constellation of light freckles across the nose and cheekbones",
	"a single dimple on the left cheek that shows when smiling",
	"long curling eyelashes that catch light",
	"a soft cupid's-bow on the upper lip",
	"an asymmetric raised eyebrow at rest, full of personality",
	"a small mole at the corner of the mouth",
	"a faint natural blush at the apples of the cheeks",
	"plush soft lips with a gentle natural shape",
	"a small delicate gold stud earring",
	"a soft pink flush across the cheeks and nose bridge",
	"a sweet softly rounded chin",
}
var visualGlasses = []string{
	"no glasses",
	"thin gold wire-frames worn low on the bridge",
	"small round John-Lennon-style wire-frames",
	"narrow rectangular tortoiseshell frames",
	"no glasses",
	"oversized rounded clear-acetate frames",
	"no glasses",
	"thin matte-black wire-frames",
	"reading glasses pushed up into the hair",
	"no glasses",
}
var visualAccessory = []string{
	"a single small gold stud earring on one ear",
	"a delicate silver chain holding a small charm",
	"a thin silk scarf knotted loosely at the neck",
	"no notable accessory",
	"a knit beanie pushed back from the forehead",
	"a graphite pencil tucked behind one ear",
	"no notable accessory",
	"a slim leather watch peeking from a sleeve",
	"an asymmetric pair of small hoop earrings",
	"a tiny enamel pin shaped like a pear on the collar",
	"a piece of red yarn wrapped twice around the wrist",
	"a single oversized ring on the index finger",
}
var visualWardrobeFeminine = []string{
	"a soft pastel-pink cardigan with pearl buttons over a fine white tee",
	"a butter-yellow puff-sleeve blouse, fresh and playful",
	"a sweet cream knit with a delicate scalloped neckline",
	"a soft lavender blouse with a small ribbon bow at the collar",
	"a pretty floral-print summer dress with a soft round neckline",
	"a fitted ribbed sage-green knit top with cap sleeves",
	"a delicate white blouse with lace trim at the collar",
	"a soft baby-blue cardigan over a pretty camisole",
	"a fresh peach-pink puff-sleeve top, sweet and youthful",
	"a clean white blouse with a pussy-bow tie at the neck",
	"a soft cream off-shoulder knit, gently feminine",
	"a pretty pleated lavender dress with a fitted bodice",
	"a soft yellow sundress with thin straps and a sweetheart neckline",
	"a pretty pastel-pink ribbed knit with a delicate scoop neck",
}
var visualWardrobeMasculine = []string{
	"a soft cream knit sweater over a clean white tee, fashion-student feel",
	"a vintage band tee under an unbuttoned plaid overshirt",
	"a clean white oxford with the sleeves rolled up to the elbow",
	"a soft cropped vintage leather jacket over a clean tee",
	"a structured navy blazer over a fine white tee, sharp and modern",
	"a sage linen shirt with the top buttons casually open",
	"a cropped corduroy jacket over a black turtleneck",
	"a fitted black turtleneck, minimalist and clean",
	"a relaxed-fit white t-shirt under a soft denim chore jacket",
	"a soft camel cashmere cardigan over a fine ribbed tee",
	"a vintage Breton stripe top, classic and cool",
	"a fresh light-blue oxford with the collar relaxed",
}
var visualWardrobeAndrogynous = []string{
	"a soft oversized cream knit, minimalist and modern",
	"a soft camel cashmere cardigan over a fine ribbed tee",
	"a clean white oxford with the collar slightly relaxed",
	"a sage linen shirt with the top buttons casually open",
	"a soft beige sweatshirt with a relaxed neckline",
	"an oversized chambray shirt worn loose, effortlessly cool",
	"a fine fawn merino crewneck over a thin white tee",
}
var visualArtStyle = []string{
	"soft watercolor portrait on heavy cotton paper, visible grain, gentle pencil under-drawing showing through, restrained brushwork",
	"crisp ink-line illustration with translucent gouache washes, magazine-cover feel, hand-lettered sensibility",
	"mid-century editorial illustration, geometric and graphic, flat shading with two or three accent colors, vintage Saul Bass / Paul Rand energy",
	"risograph-style portrait, two-color halftone (terracotta and slate), slight registration offset, pulpy paper texture",
	"oil painting feel with visible brush direction, soft impasto on the cheekbones, refined classical portrait sensibility",
	"pencil drawing with subtle digital color washes, photorealistic linework, almost monochrome with one warm color accent",
	"modern digital editorial portrait, flat color shapes with one rim-light, very pared-back, contemporary brand-illustration style",
	"soft chalk pastel on tinted paper, smudgy edges, gentle warmth, slightly impressionistic",
	"pen-and-ink line drawing with NO color, fine cross-hatching, archival-portrait sensibility",
	"gentle gouache with deliberate visible brush strokes, slightly naive proportions, contemporary picture-book sensibility",
}
var visualBackground = []string{
	"warm beige paper with subtle grain",
	"soft sage with a hint of paper texture",
	"pale terracotta gradient",
	"cool slate with paper grain",
	"cream with subtle warm texture",
	"soft peach wash",
	"muted lavender mist",
	"matte off-white",
	"warm cinnamon haze",
	"pale ochre with grain",
	"dusty teal flat color",
	"deep plum behind a soft halo of light",
}
var visualVibe = []string{
	"mid-laugh, eyes crinkling into a real smile",
	"a playful smirk just starting at the corner of the mouth",
	"quietly amused, eyes nearly closing into a smile",
	"bright open joy, head tilted with delight",
	"a knowing grin, eyes alive with mischief",
	"caught mid-thought with a small private smile",
	"warm and engaged, eyes catching the viewer",
	"cool confidence with a slight raised eyebrow, charming and direct",
	"a delighted laugh frozen mid-moment, teeth showing",
	"sweet shy smile, eyes meeting the camera then glancing away",
	"about-to-say-something energy, lips just parting",
	"a soft chic smile with sparkle in the eyes",
}
var visualHeadAngle = []string{
	"three-quarter angle to the left, head tilted with curiosity, gaze meeting the camera",
	"three-quarter angle to the right, mid-turn as if just noticed something",
	"nearly frontal with a playful head tilt and a soft smile",
	"looking back over the shoulder with a quick warm glance",
	"leaning forward a little, elbows implied, fresh open gaze",
	"head tipped slightly back in mid-laugh, eyes crinkled",
	"almost-frontal, head cocked to the side with a small smile",
	"caught looking up from below with a bright surprised expression",
	"three-quarter angle, hand-near-chin pose, thinking-but-amused",
	"profile turning toward the camera with a soft natural smile",
}

/* ───────────── hash / 选择器 ───────────── */

func pickFromHash[T any](arr []T, h uint32, salt uint32) T {
	return arr[(h^salt)%uint32(len(arr))]
}

type avatarGender string

const (
	genderFeminine    avatarGender = "feminine"
	genderMasculine   avatarGender = "masculine"
	genderAndrogynous avatarGender = "androgynous"
)

type visualSignature struct {
	Age          string
	Presentation string
	Skin         string
	HairColor    string
	HairStyle    string
	Signature    string
	Glasses      string
	Accessory    string
	Wardrobe     string
	ArtStyle     string
	Background   string
	Vibe         string
	HeadAngle    string
	Gender       avatarGender
}

func visualSignatureFor(agentID string, gender avatarGender) visualSignature {
	h := agent.HashStrJS(agentID)
	var presentationPool, hairStylePool, wardrobePool []string
	switch gender {
	case genderFeminine:
		presentationPool, hairStylePool, wardrobePool = visualPresentationFeminine, visualHairStyleFeminine, visualWardrobeFeminine
	case genderMasculine:
		presentationPool, hairStylePool, wardrobePool = visualPresentationMasculine, visualHairStyleMasculine, visualWardrobeMasculine
	default:
		presentationPool, hairStylePool, wardrobePool = visualPresentationAndrogynous, visualHairStyleAndrogynous, visualWardrobeAndrogynous
	}
	return visualSignature{
		Age:          pickFromHash(visualAge, h, 0x9E3779B9),
		Presentation: pickFromHash(presentationPool, h, 0x85EBCA6B),
		Skin:         pickFromHash(visualSkin, h, 0xC2B2AE35),
		HairColor:    pickFromHash(visualHairColor, h, 0x27D4EB2F),
		HairStyle:    pickFromHash(hairStylePool, h, 0x165667B1),
		Signature:    pickFromHash(visualSignaturePool, h, 0x3A4F1B7D),
		Glasses:      pickFromHash(visualGlasses, h, 0xD8163841),
		Accessory:    pickFromHash(visualAccessory, h, 0xA1B5E5A7),
		Wardrobe:     pickFromHash(wardrobePool, h, 0x53C5DA4D),
		ArtStyle:     pickFromHash(visualArtStyle, h, 0xB7E15162),
		Background:   pickFromHash(visualBackground, h, 0x6F4A7E13),
		Vibe:         pickFromHash(visualVibe, h, 0xE9B5DBE1),
		HeadAngle:    pickFromHash(visualHeadAngle, h, 0xCB1AB31F),
		Gender:       gender,
	}
}

// inferAgentGender:小模型分类器决定性别呈现;失败退回按名字哈希的
// 确定性二选一(绝不默认 androgynous —— 该分支刻意稀有)。
func (s *Domain) cliInferAgentGender(ctx context.Context, name, role, systemPrompt string, tenant string) avatarGender {
	hashFallback := genderMasculine
	if agent.HashStrJS(name)&1 == 0 {
		hashFallback = genderFeminine
	}
	agentArg, tenantArg := name, tenant
	obs.RecordLlmCall(s.DB, obs.LlmCallRecord{
		Purpose: "gender", CompanyID: &tenantArg, AgentID: nil, Source: "cloud",
		Model: agent.SupportModelEnv(), LatencyMS: 0, Status: "ok",
		Extras: map[string]any{"agentName": agentArg, "role": agent.TruncateRunesSimple(role, 60)},
	})
	res, err := s.ResponsesCreate(ctx, tenant, agent.CliResponsesArgs{
		Model: agent.SupportModelEnv(),
		Instructions: `Reply with strict JSON only: {"gender": "feminine" | "masculine"}, or "androgynous" only in the rare case below.

Strongly prefer feminine or masculine. Decide primarily by the NAME's cultural convention (e.g. "Atlas" / "Bram" → masculine; "Iris" / "Maya" → feminine). If the name is unisex (e.g. "Quinn", "Sky", "Riley"), use the persona / role text to break the tie. If it still leans either way at all, pick that side.

Only return "androgynous" when the name is an abstract / brand-style codename with no human gender association (e.g. "Nimbus", "Helix", "Vector") AND the persona text gives no human gender cue. This should be rare.

No prose, no explanation.`,
		Input: fmt.Sprintf("Classify the agent below and reply as JSON.\n\nName: %s\nRole: %s\nPersona / style:\n%s",
			name, orNone(role), orNone(agent.TruncateRunesSimple(systemPrompt, 500))),
		MaxOutputTokens: 200,
		JSONMode:        true,
		ReasoningEffort: "low",
	})
	if err != nil {
		return hashFallback
	}
	var parsed struct {
		Gender string `json:"gender"`
	}
	if json.Unmarshal([]byte(res.OutputText), &parsed) != nil {
		return hashFallback
	}
	switch parsed.Gender {
	case "feminine":
		return genderFeminine
	case "masculine":
		return genderMasculine
	case "androgynous":
		return genderAndrogynous
	}
	return hashFallback
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

/* ───────────── 头像生成/安装 ───────────── */

// cliGenerateAndPersistAvatar:router.ts generateAndPersistAvatar —— 视觉
// 签名确定性于 id+gender;image API → avatars/ 存储 → participants.
// avatar_url → persona 缓存失效 → CH_STATUS 广播。失败抛错由命令层接。
// GenerateAgentAvatar:admin 头像生成路由的注入面(domains/devtools 钩子;
// boards 的 WakeMentioned 同款依赖倒置)。退化图像分支文本按 admin 路径
// 的 TS 语义翻译:router.ts 抛 'image API returned no image',而共享的
// CLI 生成器沿 cli.ts 说 'no data'(#107 评审 NIT4)。
func (s *Domain) GenerateAgentAvatar(ctx context.Context, agentID, tenant string) (string, error) {
	url, err := s.cliGenerateAndPersistAvatar(ctx, agentID, tenant)
	if err != nil && err.Error() == "image API returned no data" {
		return "", fmt.Errorf("image API returned no image")
	}
	return url, err
}

func (s *Domain) cliGenerateAndPersistAvatar(ctx context.Context, agentID, tenant string) (string, error) {
	var name string
	var role sql.NullString
	var systemPrompt sql.NullString
	var kind string
	err := s.DB.QueryRowContext(ctx,
		`SELECT name, role, system_prompt, kind FROM participants WHERE id = $1 AND company_id = $2`,
		agentID, tenant).Scan(&name, &role, &systemPrompt, &kind)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("not found")
	}
	if err != nil {
		// TS:非缺失错误原样上抛 → admin 路由落 502 image generation
		// failed(瞬态 DB 故障不得伪装成 404,#107 评审 MINOR1)。
		return "", err
	}
	if kind != "agent" {
		return "", fmt.Errorf("avatar generation is only for agents")
	}
	styleHint := ""
	if systemPrompt.Valid {
		styleHint = agent.TruncateRunesSimple(systemPrompt.String, 500)
	}
	roleStr := ""
	if role.Valid {
		roleStr = role.String
	}
	gender := s.cliInferAgentGender(ctx, name, roleStr, styleHint, tenant)
	visual := visualSignatureFor(agentID, gender)
	var genderClause string
	switch gender {
	case genderFeminine:
		genderClause = fmt.Sprintf("%s is a young woman with softly feminine features and styling — a pretty face, gentle expression, distinctly girlish attire (a dress, soft blouse, pastel knit, or similarly feminine piece). Long or shoulder-length soft hair.", name)
	case genderMasculine:
		genderClause = fmt.Sprintf("%s is a young man with clearly masculine features and presentation.", name)
	default:
		genderClause = fmt.Sprintf("%s's gender presentation is intentionally androgynous and refined.", name)
	}
	roleTxt := ""
	if role.Valid {
		roleTxt = role.String
	}
	promptLines := []string{
		fmt.Sprintf("Draw %s — a young, striking, characterful individual. Beautiful in a distinctive, interesting way (not a generic \"model type\", not a \"professional headshot\"). The kind of person you'd notice in a coffee shop because their face has personality. Caught in a candid, playful moment — alive and engaged with the world, not posing stiffly. %s", name, genderClause),
		"",
		fmt.Sprintf("Who %s is, physically and personality-wise. EVERY detail below is required and must be visible in the portrait:", name),
		fmt.Sprintf("• Apparent age: %s years old (look this age, no younger, no older)", visual.Age),
		fmt.Sprintf("• Build / presentation: %s", visual.Presentation),
		fmt.Sprintf("• Skin: %s", visual.Skin),
		fmt.Sprintf("• Hair: %s — colored %s", visual.HairStyle, visual.HairColor),
		fmt.Sprintf("• A distinctive feature: %s  ← include this clearly", visual.Signature),
		fmt.Sprintf("• Eyewear: %s", visual.Glasses),
		fmt.Sprintf("• Wearing: %s", visual.Wardrobe),
		fmt.Sprintf("• Accessory: %s", visual.Accessory),
		"",
		"Pose & emotional tone (matters as much as the face):",
		fmt.Sprintf("• Framing: %s", visual.HeadAngle),
		fmt.Sprintf("• Vibe right now: %s", visual.Vibe),
	}
	if roleTxt != "" {
		promptLines = append(promptLines, fmt.Sprintf("• Their role: %s", roleTxt))
	}
	if styleHint != "" {
		promptLines = append(promptLines, fmt.Sprintf("• Inner essence the face should hint at: %s", agent.TruncateRunesSimple(styleHint, 240)))
	}
	promptLines = append(promptLines,
		"",
		"RENDER STYLE — important, this is what gives the portrait its distinct artistic identity (do NOT default to a \"minimalist editorial\" baseline):",
		fmt.Sprintf("%s.", visual.ArtStyle),
		fmt.Sprintf("Background: %s.", visual.Background),
		"",
		"Skin finish — NATURAL, not oily:",
		"- Skin should look like real human skin in honest light. Not shiny, not wet, not \"glass-skin\", not airbrushed. A real complexion with subtle texture.",
		"- No makeup highlighter, no glossy lip highlights, no contouring. Brows and lashes natural, eyes alive on their own.",
		"- Hair has a normal soft texture — no gel, no wet-look, no shellac.",
		"- Tactile texture of the medium (paper grain, brushwork) belongs in the BACKGROUND and clothing, not as bright highlights on the face.",
		"",
		"Hard rules:",
		"- Single subject, head-and-shoulders, square frame, head centered so a circular crop works.",
		fmt.Sprintf("- This is %s, not a stand-in. Lean hard into the specific features above; do not soften them toward an average.", name),
		"- The portrait MUST read as YOUNG (early 20s to early 30s) — never 35+, never \"weathered\", never \"lived-in\". If the face looks middle-aged, you have failed.",
		"- The pose must feel ALIVE and CANDID — caught mid-moment, with motion or personality. Never stiff, never \"official headshot\", never \"model gaze\".",
		"- Beautiful but DISTINCTIVE — striking features, real personality. Avoid generic \"average attractive face\".",
		"- Respect the gender presentation above; do not contradict it.",
		"- No text. No logos. No background figures. No stock-photo realism. No anime / chibi / exaggerated cartoon. No sultry / smoldering / \"Tom Ford\" glamour.",
		"- The portrait should feel like it was drawn for a profile in a magazine that cares deeply about who this person is — Refinery29 / Vice / Kinfolk / Cereal magazine youth-feature energy.",
	)
	prompt := strings.Join(filterTruthy(promptLines), "\n")

	t0 := time.Now()
	agentArg, tenantArg := agentID, tenant
	record := func(status string, errMsg *string) {
		obs.RecordLlmCall(s.DB, obs.LlmCallRecord{
			Purpose: "avatar-image", CompanyID: &tenantArg, AgentID: &agentArg, Source: "cloud",
			Model: agent.ImageModelEnv(), LatencyMS: agent.MsSince(t0), Status: status, Error: errMsg,
			Extras: map[string]any{"gender": string(gender), "kind": kind},
		})
	}
	buf, err := s.ImagesGenerate(ctx, tenant, agent.ImageModelEnv(), prompt, "1024x1024")
	if err != nil {
		msg := err.Error()
		record("failed", &msg)
		return "", err
	}
	record("ok", nil)
	key := fmt.Sprintf("avatars/avatar-%s-%s.png", agentID, agent.UUIDHex()[:8])
	url, err := agent.StoragePut(key, buf)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE participants SET avatar_url = $2 WHERE id = $1 AND company_id = $3`, agentID, url, tenant); err != nil {
		return "", err
	}
	agent.InvalidatePersonaCache(agentID)
	_ = s.PublishRaw(events.ChStatus, agent.MustJSON(map[string]any{
		"type":          "participants.avatar",
		"participantId": agentID,
		"avatarUrl":     url,
		"companyId":     tenant,
	}))
	return url, nil
}

func filterTruthy(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// cliSetAgentAvatarFromUrl:抓取任意 http(s) 图片,校验后转存到我们的
// avatars/ 存储再落 avatar_url(规范副本永远住自家存储)。
func (s *Domain) cliSetAgentAvatarFromUrl(ctx context.Context, agentID, tenant, sourceURL string) (string, error) {
	if !agent.HTTPPrefixRe.MatchString(sourceURL) {
		return "", fmt.Errorf("avatar source must be an http(s) URL")
	}
	const maxBytes = 8 * 1024 * 1024
	fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := agent.HTTPClientLLM.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("source URL returned %d", resp.StatusCode)
	}
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("content-type"), ";", 2)[0]))
	if !strings.HasPrefix(mime, "image/") {
		display := mime
		if display == "" {
			display = "unknown"
		}
		return "", fmt.Errorf("source URL is not an image (content-type: %s)", display)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", err
	}
	if len(buf) == 0 {
		return "", fmt.Errorf("source image is empty")
	}
	if len(buf) > maxBytes {
		return "", fmt.Errorf("source image too large (%d > %d)", len(buf), maxBytes)
	}
	ext := "png"
	switch mime {
	case "image/jpeg":
		ext = "jpg"
	case "image/webp":
		ext = "webp"
	case "image/gif":
		ext = "gif"
	case "image/svg+xml":
		ext = "svg"
	}
	key := fmt.Sprintf("avatars/avatar-%s-%s.%s", agentID, agent.UUIDHex()[:8], ext)
	url, err := agent.StoragePut(key, buf)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE participants SET avatar_url = $2 WHERE id = $1 AND company_id = $3`, agentID, url, tenant); err != nil {
		return "", err
	}
	agent.InvalidatePersonaCache(agentID)
	_ = s.PublishRaw(events.ChStatus, agent.MustJSON(map[string]any{
		"type":          "participants.avatar",
		"participantId": agentID,
		"avatarUrl":     url,
		"companyId":     tenant,
	}))
	return url, nil
}

/* ───────────── 命令面 ───────────── */

func (s *Domain) CmdAvatar(ctx context.Context, parsed agent.Parsed) agent.Result {
	op := ""
	if len(parsed.Positional()) > 0 {
		op = parsed.Positional()[0]
	}
	if op != "regen" && op != "regenerate" && op != "set" && op != "show" {
		return agent.Err(strings.Join([]string{
			"usage:",
			`  avatar show <participant_id>        view a teammate's portrait URL (download + open it to actually see the image)`,
			`  avatar regen [--as <id>]            regenerate your portrait from your persona`,
			`  avatar set <image_url> [--as <id>]  adopt an existing image URL as your portrait`,
		}, "\n"))
	}
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.Err(err.Error())
	}
	var tenant, kind string
	qerr := s.DB.QueryRowContext(ctx,
		`SELECT company_id, kind FROM participants WHERE id = $1`, me).Scan(&tenant, &kind)
	if qerr != nil {
		return agent.Err(fmt.Sprintf("unknown participant %s", me))
	}
	// show 只读且对 human 开放;regen/set 改自己的头像,仅 agent。
	if op != "show" && kind != "agent" {
		return agent.Err("avatar ops are only for agents")
	}

	switch op {
	case "show":
		target := ""
		if len(parsed.Positional()) > 1 {
			target = parsed.Positional()[1]
		}
		if target == "" {
			return agent.Err("usage: avatar show <participant_id>")
		}
		var id, name string
		var role sql.NullString
		var tKind string
		var avatarURL sql.NullString
		err := s.DB.QueryRowContext(ctx, `
			SELECT id, name, role, kind, avatar_url FROM participants
			 WHERE id = $1 AND company_id = $2 AND departed_at IS NULL`, target, tenant).
			Scan(&id, &name, &role, &tKind, &avatarURL)
		if err != nil {
			return agent.Err(fmt.Sprintf("unknown participant %s in this workspace", target))
		}
		who := fmt.Sprintf("%s (%s) — %s", name, id, tKind)
		if role.Valid && role.String != "" {
			who += fmt.Sprintf(", %s", role.String)
		}
		if !avatarURL.Valid || avatarURL.String == "" {
			return agent.OK(fmt.Sprintf("%s\n(no avatar set)", who))
		}
		return agent.OK(fmt.Sprintf(
			"%s\navatar URL: %s\n\nTo actually SEE the image, save it locally then open it with your image-reading tool:\n  curl -sL '%s' -o /tmp/%s-avatar\nthen open `/tmp/%s-avatar` with your Read / view-image tool.",
			who, avatarURL.String, avatarURL.String, id, id))

	case "set":
		url := ""
		if len(parsed.Positional()) > 1 {
			url = parsed.Positional()[1]
		}
		if url == "" {
			return agent.Err("usage: avatar set <image_url> [--as <id>]")
		}
		resultURL, serr := s.cliSetAgentAvatarFromUrl(ctx, me, tenant, url)
		if serr != nil {
			return agent.Err(fmt.Sprintf("avatar set failed: %s", serr.Error()))
		}
		return agent.OK(fmt.Sprintf("portrait set → %s", resultURL), agent.CliSideEffect{
			"event":     "avatar.updated",
			"command":   "avatar set",
			"agentId":   me,
			"companyId": tenant,
			"avatarUrl": resultURL,
		})

	default: // regen / regenerate
		resultURL, gerr := s.cliGenerateAndPersistAvatar(ctx, me, tenant)
		if gerr != nil {
			return agent.Err(fmt.Sprintf("avatar regen failed: %s", gerr.Error()))
		}
		return agent.OK(fmt.Sprintf("new portrait → %s", resultURL), agent.CliSideEffect{
			"event":     "avatar.updated",
			"command":   "avatar regen",
			"agentId":   me,
			"companyId": tenant,
			"avatarUrl": resultURL,
		})
	}
}

/* ───────────── image generate ───────────── */

// cliCmdImage:`image generate` —— 生成图不进会话,先给 agent 看结果,
// 满意再 --attach 进 reply。租户级 claim 防同伴重复烧同一想法。
func (s *Domain) CmdImage(ctx context.Context, parsed agent.Parsed) agent.Result {
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.Err(err.Error())
	}
	op := ""
	if len(parsed.Positional()) > 0 {
		op = parsed.Positional()[0]
	}
	if op != "generate" {
		return agent.Err(`usage: image generate "<prompt>" [--size square|wide|tall] [--as <id>] [--json]`)
	}
	prompt := strings.TrimSpace(strings.Join(parsed.Positional()[1:], " "))
	if prompt == "" {
		return agent.Err("image generate requires a non-empty prompt")
	}
	size := parsed.FlagStrOr("size", "square")

	var tenant sql.NullString
	_ = s.DB.QueryRowContext(ctx,
		`SELECT company_id FROM participants WHERE id = $1 LIMIT 1`, me).Scan(&tenant)
	tenantID := ""
	if tenant.Valid {
		tenantID = tenant.String
	}
	if tenantID != "" {
		if blocked := s.TryClaimTenantWork(tenantID, me, "image-generate", prompt); blocked != nil {
			return *blocked
		}
	}
	defer func() {
		if tenantID != "" {
			s.ReleaseWork("tenant:"+tenantID, me, "image-generate", prompt)
		}
	}()

	att, gerr := s.GenerateAndUploadImage(prompt, size, tenantID, me)
	if gerr != nil {
		return agent.Err(fmt.Sprintf("image generation failed: %s", gerr.Error()))
	}
	if parsed.FlagTruey("json") {
		txt, jerr := agent.JSONStringify(att)
		if jerr != nil {
			return agent.ErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return agent.OK(txt)
	}
	dim := "1024×1024"
	if size == "wide" {
		dim = "1536×1024"
	} else if size == "tall" {
		dim = "1024×1536"
	}
	kb := int64(0)
	if att.Size != nil {
		kb = *att.Size / 1024
		if rem := (*att.Size % 1024) * 2; rem >= 1024 {
			kb++
		}
	}
	return agent.OK(strings.Join([]string{
		fmt.Sprintf("generated %s · %dKB · %s", dim, kb, agent.ImageModelEnv()),
		fmt.Sprintf("name: %s", att.Name),
		fmt.Sprintf("url:  %s", att.URL),
		fmt.Sprintf("key:  %s", agent.DerefStr(att.Key)),
		"",
		"attach to a reply with:",
		fmt.Sprintf("  cumora reply <convo_id> \"<body>\" --attach \"%s\" --attach-name \"%s\"", att.URL, att.Name),
	}, "\n"))
}

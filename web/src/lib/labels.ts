import type { EventStoryDetail, TranslationEntry } from "./api";

// ---- Display labels (ported from the legacy client) ----

export const CATEGORY_LABELS: Record<string, string> = {
  cards: "卡牌", events: "活动", music: "音乐", gacha: "卡池",
  virtualLive: "虚拟Live", sticker: "贴纸", comic: "漫画",
  mysekai: "我的世界", costumes: "服装", characters: "角色", units: "团体",
  eventStory: "活动剧情",
};

export const FIELD_LABELS: Record<string, string> = {
  prefix: "卡面名称", skillName: "技能名", gachaPhrase: "抽卡台词",
  name: "名称", title: "标题", artist: "音乐人", vocalCaption: "歌手名",
  fixtureName: "家具名", flavorText: "描述文本", genre: "分类", tag: "标签",
  colorName: "配色名", designer: "设计师", hobby: "爱好", specialSkill: "特技",
  favoriteFood: "喜欢的食物", hatedFood: "讨厌的食物", weak: "弱点",
  introduction: "自我介绍", unitName: "团体名", profileSentence: "团体简介",
  subGenre: "子分类", material: "材料",
};

export const SOURCE_LABELS: Record<string, string> = {
  cn: "官方", human: "人工", pinned: "锁定", llm: "AI", unknown: "未知",
};

// Source ordering for the entry list: needs-work first.
export const SOURCE_ORDER: Record<string, number> = {
  unknown: 0, llm: 1, human: 2, pinned: 3, cn: 4,
};

// ---- Public site detail links ----

const SOURCE_BASE = (process.env.NEXT_PUBLIC_PJSK_BASE || "https://pjsk.moe").replace(/\/+$/, "");

export const DETAIL_BUILDERS: Record<string, (id: string) => string> = {
  cards: (id) => `${SOURCE_BASE}/cards/${id}/`,
  events: (id) => `${SOURCE_BASE}/events/${id}/`,
  gacha: (id) => `${SOURCE_BASE}/gacha/${id}/`,
  virtualLive: (id) => `${SOURCE_BASE}/live/${id}/`,
  music: (id) => `${SOURCE_BASE}/music/${id}/`,
  mysekai: (id) => `${SOURCE_BASE}/mysekai/${id}/`,
  costumes: (id) => `${SOURCE_BASE}/costumes/${id}/`,
  characters: (id) => `${SOURCE_BASE}/character/${id}/`,
};
export const EVENTSTORY_DETAIL = (id: string) => `${SOURCE_BASE}/eventstory/${id}/`;

/**
 * Build a Moesekai detail-page URL for the given category and entry key.
 * For eventStory, the field (eventId) is used directly.
 * For other categories, we try to extract a numeric ID from the jp key.
 * Returns null if no detail page is available for this category/field.
 */
export function buildMoesekaiUrl(category: string, field: string, entryKey: string): string | null {
  if (category === "eventStory") {
    return EVENTSTORY_DETAIL(field);
  }
  const builder = DETAIL_BUILDERS[category];
  if (!builder) return null;
  // Try to extract a numeric ID from the key (e.g. "3カード" → "3").
  const m = entryKey.match(/^(\d+)/);
  if (m) return builder(m[1]);
  return null;
}

// ---- Event story entry key encoding (ported from legacy) ----

export const EVENT_STORY_TITLE_MARKER = "__title__";

export function normalizeEventStorySource(source: string | undefined): string {
  switch ((source || "").trim().toLowerCase()) {
    case "official_cn":
    case "official_cn_legacy":
    case "cn":
      return "cn";
    case "llm":
      return "llm";
    case "human":
    case "pinned":
    case "unknown":
      return (source || "unknown").trim().toLowerCase();
    default:
      return "unknown";
  }
}

export function buildEventStoryEntries(detail: EventStoryDetail): TranslationEntry[] {
  const storySource = normalizeEventStorySource(detail.meta?.source);
  const entries: TranslationEntry[] = [];
  Object.entries(detail.episodes)
    .sort((a, b) => Number(a[0]) - Number(b[0]))
    .forEach(([episodeNo, ep]) => {
      if ((ep.title || "").trim() !== "") {
        entries.push({
          key: `${episodeNo}|${EVENT_STORY_TITLE_MARKER}|${ep.title}`,
          text: ep.title,
          source: ep.titleSource || storySource,
        });
      }
      const talkData = ep.talkData || {};
      const speakerNames = ep.speakerNames || {};
      const keys =
        ep.talkOrder && ep.talkOrder.length > 0
          ? [...ep.talkOrder, ...Object.keys(talkData).filter((k) => !ep.talkOrder!.includes(k))]
          : Object.keys(talkData);
      keys.forEach((jp) => {
        if (!(jp in talkData)) return;
        entries.push({
          key: `${episodeNo}|${jp}`,
          text: talkData[jp],
          source: ep.talkSources?.[jp] || storySource,
          speakerName: speakerNames[jp],
        });
      });
    });
  return entries;
}

export function parseEventStoryEntryKey(key: string): {
  episodeNo: string;
  entryType: "title" | "talk";
  originalText: string;
} {
  const parts = key.split("|");
  const episodeNo = parts[0] || "";
  if (parts[1] === EVENT_STORY_TITLE_MARKER) {
    return { episodeNo, entryType: "title", originalText: parts.slice(2).join("|") || "[章节标题]" };
  }
  return { episodeNo, entryType: "talk", originalText: parts.slice(1).join("|") };
}

export function eventStoryEntryLabel(key: string): string {
  const p = parseEventStoryEntryKey(key);
  return p.entryType === "title" ? `[章节标题] ${p.originalText}` : p.originalText;
}

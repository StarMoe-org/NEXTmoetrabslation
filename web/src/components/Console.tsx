"use client";

import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useToast } from "@/app/providers";
import {
  CategoryInfo, EventStorySummary, TranslationEntry,
  clearSession, getCategories, getEntries, getEventStories, getEventStory,
  getRole, getUsername, runCNSync, triggerAIStory,
  updateEntry, updateEventStoryLine, promoteEventStoryHuman, retryEventStory, reorderEventStory,
} from "@/lib/api";
import {
  CATEGORY_LABELS, FIELD_LABELS, SOURCE_LABELS, SOURCE_ORDER,
  EVENT_STORY_TITLE_MARKER, buildEventStoryEntries, eventStoryEntryLabel, parseEventStoryEntryKey,
  buildMoesekaiUrl,
} from "@/lib/labels";
import { useSSE } from "@/lib/sse";

interface Progress { label: string; current: number; total: number }

// localStorage-backed boolean preference. Falls back gracefully on SSR.
function usePref(key: string, fallback: boolean): [boolean, (v: boolean) => void] {
  const [value, setValue] = useState(fallback);
  useEffect(() => {
    const raw = typeof window !== "undefined" ? localStorage.getItem(key) : null;
    if (raw != null) setValue(raw === "1");
  }, [key]);
  const set = useCallback((v: boolean) => {
    setValue(v);
    if (typeof window !== "undefined") localStorage.setItem(key, v ? "1" : "0");
  }, [key]);
  return [value, set];
}

// Read a JSON array from localStorage (returns [] on missing / invalid).
function useHiddenBadges(): Set<string> {
  const [set] = useState(() => {
    if (typeof window === "undefined") return new Set<string>();
    try {
      const raw = localStorage.getItem("ui.hiddenBadges");
      return new Set<string>(raw ? JSON.parse(raw) : []);
    } catch {
      return new Set<string>();
    }
  });
  return set;
}

// ---- Inline SVG icons (lucide-style, 24×24 viewBox) ----

const IconSettings = () => (
  <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>
);
const IconShield = () => (
  <svg viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>
);
const IconLogout = () => (
  <svg viewBox="0 0 24 24"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>
);
const IconChevronLeft = () => (
  <svg viewBox="0 0 24 24"><polyline points="15 18 9 12 15 6"/></svg>
);
const IconExternalLink = () => (
  <svg viewBox="0 0 24 24"><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/></svg>
);

export function Console({ onLogout }: { onLogout: () => void }) {
  const { show } = useToast();

  const [username] = useState(getUsername());
  const [role] = useState(getRole());

  const [categories, setCategories] = useState<CategoryInfo[]>([]);
  const [eventStories, setEventStories] = useState<EventStorySummary[]>([]);
  const [category, setCategory] = useState("");
  const [field, setField] = useState("");
  const [entries, setEntries] = useState<TranslationEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [query, setQuery] = useState("");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);
  const [editValue, setEditValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<Progress | null>(null);
  const editRef = useRef<HTMLTextAreaElement>(null);
  const savingRef = useRef(false);

  // ---- UI prefs ----
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [enterSaves, setEnterSaves] = usePref("ui.saveShortcut", false);
  const hiddenBadges = useHiddenBadges();

  // On first mount, collapse the sidebar by default on narrow screens.
  useEffect(() => {
    if (typeof window !== "undefined" && window.matchMedia("(max-width: 768px)").matches) {
      setSidebarOpen(false);
    }
  }, []);

  const isEventStory = category === "eventStory";

  // ---- Load categories + event stories ----
  const reloadSidebar = useCallback(() => {
    getCategories().then(setCategories).catch((e) => show(e.message, "err"));
    getEventStories().then(setEventStories).catch(() => setEventStories([]));
  }, [show]);

  useEffect(() => { reloadSidebar(); }, [reloadSidebar]);

  // ---- Load entries on selection change ----
  const loadEntries = useCallback(() => {
    if (!category || !field) return;
    setLoading(true);
    setSelectedKey(null);
    if (isEventStory) {
      getEventStory(Number(field))
        .then((detail) => {
          const list = buildEventStoryEntries(detail);
          setEntries(list);
          if (list.length) { setSelectedKey(list[0].key); setEditValue(list[0].text); }
        })
        .catch((e) => show(e.message, "err"))
        .finally(() => setLoading(false));
      return;
    }
    getEntries(category, field)
      .then((data) => {
        data.sort((a, b) => {
          const d = (SOURCE_ORDER[a.source] ?? 5) - (SOURCE_ORDER[b.source] ?? 5);
          return d !== 0 ? d : a.key.localeCompare(b.key, undefined, { numeric: true });
        });
        setEntries(data);
        if (data.length) { setSelectedKey(data[0].key); setEditValue(data[0].text); }
      })
      .catch((e) => show(e.message, "err"))
      .finally(() => setLoading(false));
  }, [category, field, isEventStory, show]);

  useEffect(() => { loadEntries(); }, [loadEntries]);

  // ---- SSE realtime ----
  useSSE((event, data) => {
    const d = data as Record<string, unknown>;
    if (event === "sync.progress" || event === "translate.progress") {
      setProgress({ label: String(d.detail ?? ""), current: Number(d.current ?? 0), total: Number(d.total ?? 0) });
      if (Number(d.current) >= Number(d.total)) setTimeout(() => setProgress(null), 1500);
    } else if (event === "entry.updated") {
      if (d.category === category && d.field === field && d.user !== username) {
        setEntries((prev) => prev.map((e) => (e.key === d.key ? { ...e, text: String(d.text), source: String(d.source) } : e)));
        show(`${d.user} 修改了一条翻译`, "ok");
      }
    } else if (event === "eventstory.updated") {
      if (isEventStory && Number(d.eventId) === Number(field) && d.user !== username) {
        loadEntries();
      }
    }
  }, true);

  // ---- Derived ----
  const filtered = useMemo(() => {
    if (!query) return entries;
    const q = query.toLowerCase();
    return entries.filter((e) =>
      isEventStory
        ? `${eventStoryEntryLabel(e.key)}\n${e.text}`.toLowerCase().includes(q)
        : e.key.toLowerCase().includes(q) || e.text.toLowerCase().includes(q),
    );
  }, [entries, query, isEventStory]);

  const selectedIndex = useMemo(
    () => (selectedKey ? filtered.findIndex((e) => e.key === selectedKey) : -1),
    [selectedKey, filtered],
  );
  const selectedEntry = filtered[selectedIndex] ?? null;

  useEffect(() => {
    if (selectedKey && editRef.current) {
      editRef.current.focus();
      editRef.current.select();
    }
  }, [selectedKey]);

  // ---- Moesekai URL for the currently selected entry ----
  const moesekaiUrl = useMemo(() => {
    if (!selectedEntry || !category || !field) return null;
    return buildMoesekaiUrl(category, field, selectedEntry.key);
  }, [selectedEntry, category, field]);

  // ---- Actions ----
  const selectField = (cat: string, f: string) => {
    setCategory(cat); setField(f); setQuery(""); setSelectedKey(null);
    if (typeof window !== "undefined" && window.matchMedia("(max-width: 768px)").matches) {
      setSidebarOpen(false);
    }
  };

  const navigate = useCallback((dir: 1 | -1) => {
    if (selectedIndex < 0) return;
    const idx = selectedIndex + dir;
    if (idx < 0 || idx >= filtered.length) return;
    const next = filtered[idx];
    setSelectedKey(next.key);
    setEditValue(next.text);
    document.querySelector(`[data-key="${CSS.escape(next.key)}"]`)?.scrollIntoView({ block: "center", behavior: "smooth" });
  }, [selectedIndex, filtered]);

  const save = useCallback(async (overrideSource?: string) => {
    if (savingRef.current || !selectedKey || !category || !field) return;
    savingRef.current = true;
    const src = overrideSource || "human";
    try {
      if (isEventStory) {
        const p = parseEventStoryEntryKey(selectedKey);
        await updateEventStoryLine(Number(field), p.episodeNo, p.entryType === "title" ? "" : p.originalText, editValue, src, p.entryType);
        setEntries((prev) => prev.map((e) =>
          e.key === selectedKey
            ? { ...e, key: p.entryType === "title" ? `${p.episodeNo}|${EVENT_STORY_TITLE_MARKER}|${editValue}` : e.key, text: editValue, source: src }
            : e));
        if (p.entryType === "title") setSelectedKey(`${p.episodeNo}|${EVENT_STORY_TITLE_MARKER}|${editValue}`);
      } else {
        await updateEntry(category, field, selectedKey, editValue, src);
        setEntries((prev) => prev.map((e) => (e.key === selectedKey ? { ...e, text: editValue, source: src } : e)));
      }
      // Advance to next.
      const idx = filtered.findIndex((e) => e.key === selectedKey);
      if (idx >= 0 && idx < filtered.length - 1) {
        const next = filtered[idx + 1];
        setSelectedKey(next.key); setEditValue(next.text);
        setTimeout(() => document.querySelector(`[data-key="${CSS.escape(next.key)}"]`)?.scrollIntoView({ block: "center", behavior: "smooth" }), 40);
      } else {
        show("已到最后一条", "ok");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "保存失败", "err");
    } finally {
      savingRef.current = false;
    }
  }, [selectedKey, category, field, editValue, filtered, isEventStory, show]);

  const onTextareaKey = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (enterSaves) {
      // Enter = save (Shift+Enter = newline)
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); save(); }
    } else {
      // Shift+Enter = save (Enter = newline, default)
      if (e.key === "Enter" && e.shiftKey) { e.preventDefault(); save(); }
    }
    if (e.key === "Escape") { setSelectedKey(null); }
    else if ((e.ctrlKey || e.metaKey) && e.key === "ArrowUp") { e.preventDefault(); navigate(-1); }
    else if ((e.ctrlKey || e.metaKey) && e.key === "ArrowDown") { e.preventDefault(); navigate(1); }
  };

  const withBusy = async (fn: () => Promise<void>) => {
    if (busy) { show("已有任务在运行", "err"); return; }
    setBusy(true);
    try { await fn(); } finally { setBusy(false); }
  };

  // Per-story AI gap-fill: translate only the currently open event story.
  const doAIStory = () => withBusy(async () => {
    try {
      const r = await triggerAIStory(Number(field), "openai") as { totalTranslated?: number; totalCandidates?: number };
      show(`AI 补充翻译完成: ${r.totalTranslated ?? 0}/${r.totalCandidates ?? 0}`, "ok");
      reloadSidebar(); loadEntries();
    } catch (e) { show(e instanceof Error ? e.message : "AI 翻译失败", "err"); }
  });

  const currentField = categories.find((c) => c.name === category)?.fields?.find((f) => f.name === field);
  const currentStory = isEventStory ? eventStories.find((s) => String(s.eventId) === field) : undefined;

  const appClass = `app${sidebarOpen ? "" : " sidebar-collapsed"}`;

  const saveKeyLabel = enterSaves ? "Enter" : "Shift+Enter";

  return (
    <div className={appClass}>
      {/* Floating button to reopen the sidebar when collapsed/hidden. */}
      {!sidebarOpen && (
        <button className="sidebar-open-btn" onClick={() => setSidebarOpen(true)} aria-label="显示侧边栏" title="显示侧边栏">☰</button>
      )}
      {/* Mobile drawer backdrop. */}
      <div className="sidebar-backdrop" onClick={() => setSidebarOpen(false)} />

      <aside className="sidebar">
        <div className="sidebar-header">
          <div className="sidebar-title-row">
            <div>
              <h1>翻译校对</h1>
              <span className="sub">{username}{role === "admin" ? " · 管理员" : ""}</span>
            </div>
            <div className="sidebar-icon-row">
              <a className="icon-btn" href="/settings" title="用户设置"><IconSettings /></a>
              {role === "admin" && <a className="icon-btn" href="/admin" title="管理设置"><IconShield /></a>}
              <button className="icon-btn" onClick={() => { clearSession(); onLogout(); }} title="退出登录"><IconLogout /></button>
              <button className="icon-btn" onClick={() => setSidebarOpen(false)} aria-label="收起侧边栏" title="收起侧边栏"><IconChevronLeft /></button>
            </div>
          </div>
        </div>

        <div className="sidebar-scroll">
          {categories.map((cat) => (
            <div className="field-group" key={cat.name}>
              <div className="field-group-title">{CATEGORY_LABELS[cat.name] || cat.name}</div>
              {cat.fields?.map((f) => {
                const work = f.llmCount + f.unknownCount;
                const active = category === cat.name && field === f.name;
                const badgeKey = `${cat.name}:${f.name}`;
                const hideBadge = hiddenBadges.has(badgeKey);
                return (
                  <div key={badgeKey} className={`field-item ${active ? "active" : ""}`} onClick={() => selectField(cat.name, f.name)}>
                    <span>{FIELD_LABELS[f.name] || f.name}</span>
                    {work > 0 && !hideBadge && <span className="badge work">{work}</span>}
                  </div>
                );
              })}
            </div>
          ))}

          {eventStories.length > 0 && (
            <div className="field-group">
              <div className="field-group-title">活动剧情 ({eventStories.length})</div>
              {eventStories.map((s) => {
                const active = category === "eventStory" && field === String(s.eventId);
                const done = s.untranslatedCount === 0;
                const badgeKey = `eventStory:${s.eventId}`;
                const hideBadge = hiddenBadges.has(badgeKey);
                return (
                  <div key={s.eventId} className={`field-item ${active ? "active" : ""}`} onClick={() => selectField("eventStory", String(s.eventId))}>
                    <span>
                      <span className={`story-dot ${done ? "done" : "pending"}`} title={done ? "已翻译" : "有未翻译内容"} />
                      Event #{s.eventId}
                    </span>
                    {!hideBadge && (
                      s.untranslatedCount > 0
                        ? <span className="badge work" title="未翻译条数">{s.untranslatedCount}</span>
                        : <span className="badge ok" title="已全部翻译">✓</span>
                    )}
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </aside>

      <main className="main">
        {progress && (
          <div className="progress-line">
            <span>{progress.label}</span>
            <div className="progress-track">
              <div className="progress-fill" style={{ width: `${progress.total ? (progress.current / progress.total) * 100 : 0}%` }} />
            </div>
          </div>
        )}

        {!category || !field ? (
          <div className="center-state">
            <p>从左侧选择一个翻译类别</p>
          </div>
        ) : (
          <>
            <div className="main-header">
              <h2>{CATEGORY_LABELS[category] || category} / {isEventStory ? `Event #${field}` : (FIELD_LABELS[field] || field)}</h2>
              <span className="count">
                {selectedIndex >= 0 ? `${selectedIndex + 1} / ` : ""}{filtered.length} 条
                {currentField && ` （共 ${currentField.total}）`}
              </span>
            </div>

            {/* Per-story toolbar */}
            {isEventStory && (
              <div className="story-toolbar">
                <span className="story-status">
                  {currentStory && currentStory.untranslatedCount > 0
                    ? <><span className="story-dot pending" /> {currentStory.untranslatedCount} 条未翻译</>
                    : <><span className="story-dot done" /> 已全部翻译</>}
                </span>
                <div className="story-toolbar-actions">
                  <button className="btn btn-primary btn-sm" onClick={doAIStory} disabled={busy}>AI 补充剧情翻译</button>
                  <button className="btn btn-secondary btn-sm" onClick={() => withBusy(async () => { await promoteEventStoryHuman(Number(field)); setEntries((p) => p.map((e) => ({ ...e, source: "human" }))); reloadSidebar(); show("已整篇标记人工", "ok"); })} disabled={busy}>整篇标记人工</button>
                  <button className="btn btn-secondary btn-sm" onClick={() => withBusy(async () => { await retryEventStory(Number(field)); loadEntries(); reloadSidebar(); show("已重新获取剧情", "ok"); })} disabled={busy}>重新获取剧情</button>
                  <button className="btn btn-secondary btn-sm" onClick={() => withBusy(async () => { await reorderEventStory(Number(field)); loadEntries(); show("已重排序对话", "ok"); })} disabled={busy}>重排序对话</button>
                </div>
              </div>
            )}

            <div className="search-bar">
              <input placeholder="搜索日文或中文…" value={query} onChange={(e) => setQuery(e.target.value)} />
            </div>

            <div className="content">
              {selectedEntry && (
                <div className="proof-panel">
                  <div className="proof-jp">
                    <span className="label">日文原文</span>
                    {selectedEntry.speakerName && <div className="speaker">{selectedEntry.speakerName}</div>}
                    <div className="jp-body">{isEventStory ? eventStoryEntryLabel(selectedEntry.key) : selectedEntry.key}</div>
                    {isEventStory && <div className="episode">第 {parseEventStoryEntryKey(selectedEntry.key).episodeNo} 章</div>}
                    {moesekaiUrl && (
                      <a className="moesekai-link" href={moesekaiUrl} target="_blank" rel="noopener noreferrer" title="在 Moesekai 上查看详情">
                        <IconExternalLink /> Moesekai 页面
                      </a>
                    )}
                  </div>
                  <div className="proof-edit">
                    <div className="proof-edit-head">
                      <span className="label">翻译校对 <span className={`source-tag ${selectedEntry.source}`}>{SOURCE_LABELS[selectedEntry.source] || selectedEntry.source}</span></span>
                      <div style={{ display: "flex", gap: 6 }}>
                        <button className="btn btn-ghost btn-sm" onClick={() => navigate(-1)} disabled={selectedIndex <= 0}>↑ 上一条</button>
                        <button className="btn btn-ghost btn-sm" onClick={() => navigate(1)} disabled={selectedIndex >= filtered.length - 1}>下一条 ↓</button>
                      </div>
                    </div>
                    <textarea
                      ref={editRef}
                      className="proof-textarea"
                      value={editValue}
                      onChange={(e) => setEditValue(e.target.value)}
                      onKeyDown={onTextareaKey}
                      placeholder="输入翻译…"
                      rows={3}
                    />
                    <div className="proof-actions">
                      <button className="btn btn-primary" onClick={() => save()}>保存并下一条</button>
                      {!isEventStory && <button className="btn btn-secondary" onClick={() => save("pinned")}>锁定保存</button>}
                      <button className="btn btn-ghost btn-sm" onClick={() => setEnterSaves(!enterSaves)} title="切换保存快捷键">
                        快捷键: {saveKeyLabel}
                      </button>
                      <div className="proof-hints">
                        <span>保存 <kbd>{saveKeyLabel}</kbd></span>
                        <span><kbd>Ctrl+↑↓</kbd> 切换</span>
                      </div>
                    </div>
                  </div>
                </div>
              )}

              {loading ? (
                <div className="center-state"><div className="spinner" />加载中…</div>
              ) : filtered.length === 0 ? (
                <div className="center-state"><p>暂无数据</p></div>
              ) : (
                <table className="entry-table">
                  <thead>
                    <tr><th className="col-source">来源</th><th>日文原文</th><th>当前翻译</th></tr>
                  </thead>
                  <tbody>
                    {filtered.map((entry) => (
                      <tr
                        key={entry.key}
                        data-key={entry.key}
                        className={`entry-row ${selectedKey === entry.key ? "active" : ""}`}
                        onClick={() => { setSelectedKey(entry.key); setEditValue(entry.text); }}
                      >
                        <td className="col-source"><span className={`source-tag ${entry.source}`}>{SOURCE_LABELS[entry.source] || entry.source}</span></td>
                        <td>
                          <div className="jp">
                            {entry.speakerName && <div className="speaker">{entry.speakerName}</div>}
                            {isEventStory ? eventStoryEntryLabel(entry.key) : entry.key}
                          </div>
                        </td>
                        <td><div className="cn">{entry.text}</div></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </>
        )}
      </main>
    </div>
  );
}

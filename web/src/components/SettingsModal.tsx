"use client";

import { useCallback, useEffect, useState } from "react";
import { useTheme } from "next-themes";
import { useToast } from "@/app/providers";
import { Modal } from "@/components/Modal";
import {
  BackupStatus, CategoryInfo, EventStorySummary, UpstreamStatus,
  getBackupStatus, getCategories, getEventStories,
  getUpstreamStatusPublic, getUsername, getRole,
  pushBackup, runCNSync,
} from "@/lib/api";
import { CATEGORY_LABELS, FIELD_LABELS } from "@/lib/labels";

type ShowFn = (msg: string, type?: "ok" | "err") => void;

export function SettingsModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { show } = useToast();
  const [upstreamRefreshKey, setUpstreamRefreshKey] = useState(0);

  return (
    <Modal open={open} onClose={onClose} title="用户设置">
      <div className="modal-cards">
        <AccountCard />
        <AppearanceCard />
        <ShortcutCard />
        <BadgeFilterCard show={show} />
        <DataManagementCard show={show} onSyncFinished={() => setUpstreamRefreshKey((v) => v + 1)} />
        <UpstreamStatusCard show={show} refreshKey={upstreamRefreshKey} />
      </div>
    </Modal>
  );
}

// ---- Account ----

function AccountCard() {
  const [username] = useState(getUsername());
  const [role] = useState(getRole());
  return (
    <div className="card">
      <h3>账户</h3>
      <table className="data-table">
        <tbody>
          <tr><th>用户名</th><td>{username || "—"}</td></tr>
          <tr><th>角色</th><td>{role === "admin" ? "管理员" : role === "editor" ? "校对员" : "—"}</td></tr>
        </tbody>
      </table>
    </div>
  );
}

// ---- Appearance (theme) ----

function AppearanceCard() {
  const { theme, setTheme } = useTheme();
  const [mounted, setMounted] = useState(false);
  useEffect(() => setMounted(true), []);

  return (
    <div className="card">
      <h3>外观</h3>
      <div className="form-row">
        <label>主题</label>
        {mounted ? (
          <select value={theme} onChange={(e) => setTheme(e.target.value)}>
            <option value="system">跟随系统</option>
            <option value="light">亮色</option>
            <option value="dark">深色</option>
          </select>
        ) : (
          <select disabled><option>加载中…</option></select>
        )}
      </div>
    </div>
  );
}

// ---- Shortcut settings ----

function ShortcutCard() {
  const [enterSaves, setEnterSaves] = useState(false);
  useEffect(() => {
    const raw = localStorage.getItem("ui.saveShortcut");
    setEnterSaves(raw === "1");
  }, []);
  const toggle = (v: boolean) => {
    setEnterSaves(v);
    localStorage.setItem("ui.saveShortcut", v ? "1" : "0");
  };

  return (
    <div className="card">
      <h3>快捷键</h3>
      <div className="form-row">
        <label>保存快捷键</label>
        <select value={enterSaves ? "enter" : "shift-enter"} onChange={(e) => toggle(e.target.value === "enter")}>
          <option value="shift-enter">Shift+Enter 保存（默认）</option>
          <option value="enter">Enter 保存</option>
        </select>
      </div>
      <table className="data-table">
        <thead><tr><th>快捷键</th><th>功能</th></tr></thead>
        <tbody>
          <tr><td><kbd>{enterSaves ? "Enter" : "Shift+Enter"}</kbd></td><td>保存并下一条</td></tr>
          <tr><td><kbd>Escape</kbd></td><td>取消选中</td></tr>
          <tr><td><kbd>Ctrl+↑</kbd> / <kbd>Ctrl+↓</kbd></td><td>切换上/下一条目</td></tr>
        </tbody>
      </table>
    </div>
  );
}

// ---- Badge filter (per-field hide) ----

function BadgeFilterCard({ show }: { show: ShowFn }) {
  const [categories, setCategories] = useState<CategoryInfo[]>([]);
  const [eventStories, setEventStories] = useState<EventStorySummary[]>([]);
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    Promise.all([
      getCategories().catch(() => [] as CategoryInfo[]),
      getEventStories().catch(() => [] as EventStorySummary[]),
    ]).then(([cats, stories]) => {
      setCategories(cats);
      setEventStories(stories);
      try {
        const raw = localStorage.getItem("ui.hiddenBadges");
        setHidden(new Set(raw ? JSON.parse(raw) : []));
      } catch { /* ignore */ }
      setLoaded(true);
    });
  }, []);

  const persist = useCallback((next: Set<string>) => {
    setHidden(next);
    localStorage.setItem("ui.hiddenBadges", JSON.stringify([...next]));
  }, []);

  const toggle = (key: string) => {
    const next = new Set(hidden);
    if (next.has(key)) next.delete(key); else next.add(key);
    persist(next);
  };

  const allKeys = [
    ...categories.flatMap((c) => c.fields.map((f) => `${c.name}:${f.name}`)),
    ...eventStories.map((s) => `eventStory:${s.eventId}`),
  ];
  const allHidden = allKeys.length > 0 && allKeys.every((k) => hidden.has(k));

  const selectAll = () => persist(new Set(allKeys));
  const selectNone = () => persist(new Set());

  if (!loaded) return <div className="card"><h3>角标显示</h3><div className="center-state" style={{ height: 60 }}><div className="spinner" /></div></div>;

  return (
    <div className="card">
      <h3>角标显示</h3>
      <p style={{ fontSize: 13, color: "var(--text-secondary)", marginBottom: 12 }}>
        勾选要隐藏数量角标的分类字段。取消勾选则显示角标。
      </p>
      <div className="badge-filter-actions">
        <button className="btn btn-ghost btn-sm" onClick={selectAll} disabled={allHidden}>全部隐藏</button>
        <button className="btn btn-ghost btn-sm" onClick={selectNone} disabled={!allHidden}>全部显示</button>
      </div>
      <div className="badge-filter-list" style={{ marginTop: 8 }}>
        {categories.map((cat) =>
          cat.fields.map((f) => {
            const key = `${cat.name}:${f.name}`;
            return (
              <label className="badge-filter-item" key={key}>
                <input type="checkbox" checked={hidden.has(key)} onChange={() => toggle(key)} />
                <span>{CATEGORY_LABELS[cat.name] || cat.name} / {FIELD_LABELS[f.name] || f.name}</span>
              </label>
            );
          })
        )}
        {eventStories.map((s) => {
          const key = `eventStory:${s.eventId}`;
          return (
            <label className="badge-filter-item" key={key}>
              <input type="checkbox" checked={hidden.has(key)} onChange={() => toggle(key)} />
              <span>活动剧情 / Event #{s.eventId}</span>
            </label>
          );
        })}
      </div>
    </div>
  );
}

// ---- Data management (CN sync + manual backup) ----

function DataManagementCard({ show, onSyncFinished }: { show: ShowFn; onSyncFinished: () => void }) {
  const [busy, setBusy] = useState(false);
  const [backupStatus, setBackupStatus] = useState<BackupStatus | null>(null);
  const [loading, setLoading] = useState(true);

  const reloadBackup = useCallback(() => {
    setLoading(true);
    getBackupStatus()
      .then(setBackupStatus)
      .catch(() => setBackupStatus(null))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => { reloadBackup(); }, [reloadBackup]);

  const doSync = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const result = await runCNSync();
      if (result.skipped?.length) {
        show(`数据更新完成，但跳过: ${result.skipped.join(", ")}`, "err");
      } else {
        show("数据更新完成", "ok");
      }
    } catch (e) {
      show(e instanceof Error ? e.message : "更新失败", "err");
    } finally {
      setBusy(false);
      onSyncFinished();
    }
  };

  const doBackup = async () => {
    if (busy) return;
    setBusy(true);
    try {
      const r = await pushBackup();
      show(`备份完成: ${Object.entries(r.results).map(([k, v]) => `${k}: ${v}`).join(", ")}`, "ok");
      reloadBackup();
    } catch (e) {
      show(e instanceof Error ? e.message : "备份失败", "err");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <h3>数据管理</h3>
      <div style={{ display: "flex", gap: 8, marginBottom: 16, flexWrap: "wrap" }}>
        <button className="btn btn-primary" onClick={doSync} disabled={busy}>数据更新（CN 同步）</button>
        <button className="btn btn-secondary" onClick={doBackup} disabled={busy}>手动备份</button>
        <button className="btn btn-ghost" onClick={reloadBackup} disabled={loading}>刷新状态</button>
      </div>

      {loading && !backupStatus ? (
        <div className="center-state" style={{ height: 60 }}><div className="spinner" /></div>
      ) : backupStatus ? (
        <table className="data-table">
          <tbody>
            <tr><th>备份运行中</th><td>{backupStatus.running ? "是" : "否"}</td></tr>
            <tr><th>S3 备份</th><td>{backupStatus.s3Enabled ? "已启用" : "未配置"}</td></tr>
            <tr><th>Git 备份</th><td>{backupStatus.gitEnabled ? "已启用" : "未配置"}</td></tr>
            <tr><th>上次备份</th><td>{backupStatus.lastBackup || "—"}</td></tr>
            {backupStatus.lastS3Backup && <tr><th>上次 S3</th><td>{backupStatus.lastS3Backup}</td></tr>}
            {backupStatus.lastGitBackup && <tr><th>上次 Git</th><td>{backupStatus.lastGitBackup}</td></tr>}
            {backupStatus.lastRestore && <tr><th>上次恢复</th><td>{backupStatus.lastRestore}</td></tr>}
            {backupStatus.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{backupStatus.lastError}</td></tr>}
          </tbody>
        </table>
      ) : (
        <p style={{ color: "var(--text-dim)" }}>未配置备份</p>
      )}
    </div>
  );
}

// ---- Upstream status (read-only) ----

function UpstreamStatusCard({ show, refreshKey }: { show: ShowFn; refreshKey: number }) {
  const [status, setStatus] = useState<UpstreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const reload = useCallback(() => {
    setLoading(true);
    getUpstreamStatusPublic()
      .then(setStatus)
      .catch((e) => show(e instanceof Error ? e.message : "获取失败", "err"))
      .finally(() => setLoading(false));
  }, [show]);
  useEffect(() => { reload(); }, [reload, refreshKey]);

  return (
    <div className="card">
      <h3>上游更新状态</h3>
      {loading && !status ? (
        <div className="center-state" style={{ height: 80 }}><div className="spinner" /></div>
      ) : status ? (
        <table className="data-table">
          <tbody>
            <tr><th>启用</th><td>{status.enabled ? "是" : "否"}</td></tr>
            <tr><th>仓库</th><td>{status.repo ? `${status.repo}@${status.branch}` : "—"}</td></tr>
            <tr><th>检测源</th><td>{status.versionURL || "—"}</td></tr>
            {status.lastSource && <tr><th>实际使用源</th><td>{status.lastSource}</td></tr>}
            <tr><th>当前 dataVersion</th><td>{status.lastDataVersion || "—"}</td></tr>
            <tr><th>上次检查</th><td>{status.lastCheck || "—"}</td></tr>
            <tr><th>上次成功</th><td>{status.lastSuccess || "—"}</td></tr>
            <tr><th>上次同步</th><td>{status.lastSync || "—"}</td></tr>
            {!!status.consecutiveFailures && <tr><th>连续失败</th><td>{status.consecutiveFailures}</td></tr>}
            {status.rateLimitedUntil && <tr><th>限流冷却</th><td>{status.rateLimitedUntil}</td></tr>}
            {status.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{status.lastError}{status.lastErrorAt ? ` (${status.lastErrorAt})` : ""}</td></tr>}
          </tbody>
        </table>
      ) : (
        <p style={{ color: "var(--text-dim)" }}>暂无状态信息</p>
      )}
      <div style={{ marginTop: 12 }}>
        <button className="btn btn-secondary" onClick={reload} disabled={loading}>刷新</button>
      </div>
    </div>
  );
}

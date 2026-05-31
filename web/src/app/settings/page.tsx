"use client";

import { useCallback, useEffect, useState } from "react";
import { useTheme } from "next-themes";
import { useToast } from "@/app/providers";
import {
  UpstreamStatus,
  getRole, getToken, getUpstreamStatusPublic, getUsername,
} from "@/lib/api";

// User settings: preferences (theme), account info, and a read-only view of the
// upstream update status. Available to every logged-in user (admin or editor).
// Admin-only configuration lives separately under /admin.
export default function SettingsPage() {
  const { show } = useToast();
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [isAdmin, setIsAdmin] = useState(false);

  useEffect(() => {
    if (!getToken()) {
      setAuthed(false);
      return;
    }
    setIsAdmin(getRole() === "admin");
    setAuthed(true);
  }, []);

  if (authed === null) return <div className="center-state" style={{ height: "100vh" }}><div className="spinner" /></div>;
  if (!authed) {
    return (
      <div className="center-state" style={{ height: "100vh" }}>
        <p>请先登录</p>
        <a className="btn btn-secondary" href="/">返回控制台</a>
      </div>
    );
  }

  return (
    <div className="admin-page">
      <div className="topnav">
        <a href="/">控制台</a>
        <a href="/settings" className="active">用户设置</a>
        {isAdmin && <a href="/admin">管理设置</a>}
      </div>
      <div className="admin-body">
        <AccountCard />
        <AppearanceCard />
        <UpstreamStatusCard show={show} />
      </div>
    </div>
  );
}

type ShowFn = (msg: string, type?: "ok" | "err") => void;

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

// ---- Upstream status (read-only) ----

function UpstreamStatusCard({ show }: { show: ShowFn }) {
  const [status, setStatus] = useState<UpstreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const reload = useCallback(() => {
    setLoading(true);
    getUpstreamStatusPublic()
      .then(setStatus)
      .catch((e) => show(e instanceof Error ? e.message : "获取失败", "err"))
      .finally(() => setLoading(false));
  }, [show]);
  useEffect(() => { reload(); }, [reload]);

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
            <tr><th>当前 dataVersion</th><td>{status.lastDataVersion || "—"}</td></tr>
            <tr><th>上次检查</th><td>{status.lastCheck || "—"}</td></tr>
            <tr><th>上次同步</th><td>{status.lastSync || "—"}</td></tr>
            {status.lastError && <tr><th>错误</th><td style={{ color: "var(--err)" }}>{status.lastError}</td></tr>}
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

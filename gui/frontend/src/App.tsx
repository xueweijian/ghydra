// App.tsx —— 侧栏布局 + hash 路由（M3-W1 骨架；无路由依赖，保持轻）。
// SSE 单连接全局订阅：Status/Downloads 页消费同一 EventsState。
import { createSignal, Show, onCleanup } from "solid-js";
import { useEvents } from "./api/sse";
import Status from "./pages/Status";
import Boost from "./pages/Boost";
import Downloads from "./pages/Downloads";
import Mitm from "./pages/Mitm";
import Settings from "./pages/Settings";

type Page = "status" | "boost" | "downloads" | "mitm" | "settings";

const pages: { id: Page; label: string }[] = [
  { id: "status", label: "状态" },
  { id: "boost", label: "加速" },
  { id: "downloads", label: "下载" },
  { id: "mitm", label: "增强" },
  { id: "settings", label: "设置" },
];

function current(): Page {
  const h = location.hash.replace("#/", "");
  return (pages.find((p) => p.id === h)?.id || "status") as Page;
}

export default function App() {
  const [page, setPage] = createSignal<Page>(current());
  const [needToken, setNeedToken] = createSignal(false);
  const events = useEvents();

  window.addEventListener("hashchange", () => setPage(current()));
  window.addEventListener("ghydra.unauthorized", (() => setNeedToken(true)) as EventListener);
  onCleanup(() => events.close());

  const nav = (p: Page) => {
    location.hash = `#/${p}`;
    setPage(p);
  };

  return (
    <div class="shell">
      <aside>
        <h1>GHydra</h1>
        <nav>
          {pages.map((p) => (
            <a class={page() === p.id ? "on" : ""} href={`#/${p.id}`} onClick={() => nav(p.id)}>
              {p.label}
            </a>
          ))}
        </nav>
        <p class="muted foot">
          {events.state().connected ? "● daemon 已连接" : "○ daemon 未连接"}
        </p>
      </aside>
      <main>
        <Show when={needToken()}>
          <div class="banner">
            需要 API token——终端运行 <code>ghydra token</code>，粘贴到「设置」页。
            <button onClick={() => setNeedToken(false)}>×</button>
          </div>
        </Show>
        {page() === "status" && <Status events={events.state} />}
        {page() === "boost" && <Boost />}
        {page() === "downloads" && <Downloads events={events.state} />}
        {page() === "mitm" && <Mitm />}
        {page() === "settings" && <Settings />}
      </main>
    </div>
  );
}

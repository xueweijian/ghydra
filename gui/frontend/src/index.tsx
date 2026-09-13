/* @refresh reload */
import { render } from "solid-js/web";
import App from "./App";
import { TOKEN_KEY } from "./api/client";

// 浏览器兜底模式入口：URL 带 ?token= 自动落地（CLI 打开面板/书签场景；
// serve 同源托管，token 不出本机）。落地后清掉 URL 里的 token 防泄漏
// （历史记录/复制链接）。
{
  const q = new URLSearchParams(location.search);
  const t = q.get("token");
  if (t) {
    localStorage.setItem(TOKEN_KEY, t.trim());
    q.delete("token");
    const rest = q.toString();
    history.replaceState(null, "", location.pathname + (rest ? `?${rest}` : "") + location.hash);
  }
}

render(() => <App />, document.getElementById("root")!);

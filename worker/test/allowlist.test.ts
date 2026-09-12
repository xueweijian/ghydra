import { describe, expect, it } from "vitest";
import { allowedHosts, parseTarget } from "../src/allowlist";

const def = allowedHosts();

describe("allowedHosts", () => {
  it("默认集含 Release 资产真实域名", () => {
    expect(def.has("release-assets.githubusercontent.com")).toBe(true);
    expect(def.has("github.com")).toBe(true);
    expect(def.has("github-releases.githubusercontent.com")).toBe(true);
  });
  it("env 增补只增不删，忽略空白与大小写", () => {
    const s = allowedHosts(" MiRrOr.Example.COM , , foo.io ");
    expect(s.has("mirror.example.com")).toBe(true);
    expect(s.has("foo.io")).toBe(true);
    expect(s.has("github.com")).toBe(true);
  });
});

describe("parseTarget 防御", () => {
  it("合法：github.com + search", () => {
    const t = parseTarget("/https://github.com/o/r/releases/download/v1/x.zip?x=1", def);
    expect(t?.hostname).toBe("github.com");
    expect(t?.search).toBe("?x=1");
  });
  it("合法：host 大小写不敏感", () => {
    expect(parseTarget("/https://API.GitHub.com/zen", def)?.hostname).toBe("api.github.com");
  });
  it("合法：显式 443 端口", () => {
    expect(parseTarget("/https://github.com:443/", def)).not.toBeNull();
  });
  it("拒绝：http 明文", () => {
    expect(parseTarget("/http://github.com/", def)).toBeNull();
  });
  it("拒绝：协议相对 //evil.com", () => {
    expect(parseTarget("///evil.com/x", def)).toBeNull();
    expect(parseTarget("//evil.com/x", def)).toBeNull();
  });
  it("拒绝：非 https 前缀任意形态", () => {
    expect(parseTarget("/ftp://github.com/", def)).toBeNull();
    expect(parseTarget("/github.com/o/r", def)).toBeNull();
    expect(parseTarget("/", def)).toBeNull();
  });
  it("拒绝：userinfo 注入", () => {
    expect(parseTarget("/https://evil%40github.com@github.com/", def)).toBeNull();
    expect(parseTarget("/https://user:pass@github.com/", def)).toBeNull();
  });
  it("拒绝：非 443 端口", () => {
    expect(parseTarget("/https://github.com:8443/", def)).toBeNull();
  });
  it("拒绝：白名单外 host（含子域伪造）", () => {
    expect(parseTarget("/https://evil.com/", def)).toBeNull();
    expect(parseTarget("/https://github.com.evil.com/", def)).toBeNull();
    expect(parseTarget("/https://notgithub.com/", def)).toBeNull();
  });
  it("增补后放行", () => {
    const s = allowedHosts("mirror.example.com");
    expect(parseTarget("/https://mirror.example.com/x", s)).not.toBeNull();
  });
});

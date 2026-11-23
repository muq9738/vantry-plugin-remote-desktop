export function dashboard() {
  return { vantryPanel: { version: 1, title: "远程桌面", widgets: [
    { kind: "text", text: "局域网内可直接连接；跨网络请配置自己的中继。在目标电脑开启共享后复制连接地址。" }
  ] } };
}
export function page() { return dashboard(); }
export function run() { return dashboard(); }

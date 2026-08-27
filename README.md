# ccLoad

本仓库是基于原作者 `caidaoli` 的 [ccLoad](https://github.com/caidaoli/ccLoad) 项目 fork 的个人维护版，遵循原项目 MIT License。

主要改动：

- 新增渠道排序方案保存/切换功能
- Docker 镜像、Release 下载和自动更新链路指向本 fork
- 增强 Codex 客户端到 OpenAI/Anthropic 上游的本地协议转换
- 为 OpenAI 渠道的 Codex 本地转换提供按渠道勾选的上游能力说明
- GPT 上下文压缩会跳过模型重定向渠道，按优先级尝试同名原生 GPT 渠道
- HTTP 200 空响应不会直接结束请求，会继续尝试下一个符合条件的渠道
- 短窗口限流（如「1 分钟内最多 5 次」）按上游声明的窗口精确冷却，不再套用指数退避

## 本地协议转换

渠道编辑中的“转换方式”提供两种模式：

- `上游`：协议转换由上游接口完成，ccLoad 不改写请求和响应格式。
- `ccLoad(实验性)`：由 ccLoad 在本地转换客户端协议与上游协议。

当前本地转换重点兼容 Codex `/v1/responses` 客户端，包括：

- 普通对话、推理内容与完整流式消息事件
- function、custom、namespace 和 `tool_search` 工具声明、调用及结果续答
- 超长工具名稳定缩短，并在响应中恢复原始名称和 namespace
- Codex 到 OpenAI Chat Completions、Anthropic Messages 的请求与响应转换
- Grok 对嵌套 `oneOf` 工具 Schema 的兼容处理
- data URL 图片转 Anthropic base64 图片，HTTPS 图片保持 URL
- OpenAI/Anthropic 多文本块、工具调用和最终正文的正确回填

### Codex → OpenAI 上游能力

当渠道类型为 `OpenAI`、转换方式为 `ccLoad(实验性)`，并勾选了 `Codex` 协议转换时，渠道编辑页会显示“Codex 转 OpenAI 上游能力”：

- 函数工具 (Function Tools)
- 托管网页搜索 (Hosted Web Search)
- 工具搜索 (Tool Search)
- 推理 / Thinking (Reasoning Effort)
- 提示词缓存 (Prompt Cache)

说明：

- 能力按**渠道**配置，不会按 `grok` / `claude` 等模型名自动判断。
- 默认全部开启，兼容旧渠道；取消勾选后，ccLoad 会在转发前移除对应请求字段。
- 例如上游不支持 hosted web search 时，关闭“托管网页搜索”即可，避免把 `web_search` 原样发给不兼容接口。
- 同一会话若混用“支持工具”和“不支持工具”的渠道，不支持工具的渠道只会收到裁剪后的请求；建议把能力不同的渠道分开路由。

部分高级工具、图片格式、推理字段和上下文压缩仍取决于上游接口能力。使用 `ccLoad(实验性)` 时，如果某个渠道返回错误或没有有效正文/工具输出，ccLoad 会在允许重试的情况下继续尝试后续渠道。

Docker 镜像：

```bash
docker pull ghcr.io/hackenleung/ccload:latest
```

原项目说明和完整功能文档可参考：[caidaoli/ccLoad](https://github.com/caidaoli/ccLoad)

本 fork 的最新版本与多平台二进制文件见：[GitHub Releases](https://github.com/HackenLeung/ccLoad/releases/latest)

# Docs Seed

Docs Seed 从现有代码、Git 历史和主分支派生关系生成面向人类的项目文档。它借鉴
[Skills Seed](https://github.com/silaswei-io/skills-seed) 的增量学习与本地产物模式，
但输出目标不是 Agent Skills，而是业务逻辑和数据流转文档。

## 文档边界

Docs Seed 只回答：

- 系统实现了哪些业务规则、状态变化和业务编排。
- 数据从哪里进入，经过哪些处理，写入哪里，并如何流向外部系统。
- 异常、失败和补偿路径在业务上有什么影响。
- 源码结构、配置边界、数据所有权和流程编排已经体现了哪些架构决策。

Docs Seed 不生成函数签名、API 调用示例、CLI 命令、参数说明、安装步骤或具体代码
调用方式。ADR 结果文档只记录已有源码证据支持的决策、取舍和后果，不替团队编造未来
决策。实现细节仍以源码和 Git 历史为准。

## 分支增量模型

项目在根目录的 `.docs-seed.yml` 中配置主分支匹配规则：

```yaml
branches:
  remote: origin
  main_patterns:
    - main
    - master
    - llm/**
  parent_overrides: {}
```

`docs-seed branches sync` 先执行 `git fetch --all --prune`，然后从本地和所选远端的
refs 建立主分支谱系。谱系只依赖 Git 提交图；非主分支可以位于两个主分支之间，工具
会沿提交祖先继续回溯，直到找到匹配的父主分支。

例如主分支关系为 `A → B → C`：

- A 保存代码在 A tip 上体现的全量业务和数据流。
- B 只保存 B 相对 A fork point 的增量。
- C 只保存 C 相对 B fork point 的增量。

当 Git 证据无法唯一确定父主分支时，在 `parent_overrides` 中显式配置。工具不会让
LLM 猜测分支关系。计算结果保存在 `.docs-seed/branch-graph.json`。

## 快速开始

```bash
docs-seed init
docs-seed branches sync
docs-seed sync --evolution
```

默认 Agent 是 Claude CLI。也可以修改 `.docs-seed.yml`：

```yaml
agent:
  engine: codex
  commands:
    claude: claude
    codex: codex
  timeout_seconds: 1800
```

生成结果默认位于：

```text
.docs-seed/docs/
├── README.md
└── branches/
    ├── main/
    │   ├── README.md
    │   ├── business-logic.md
    │   ├── adr.md
    │   ├── commit-evolution.md
    │   └── data-flow.md
    └── llm__order-v2/
        ├── README.md
        ├── business-logic.md
        ├── adr.md
        ├── commit-evolution.md
        └── data-flow.md
```

分支名中的 `/` 在目录名中写为 `__`，文档正文仍保留原始分支名。

## 命令

| 命令 | 作用 |
|---|---|
| `docs-seed init` | 创建 `.docs-seed.yml` 和状态目录 |
| `docs-seed init --workspace` | 初始化多 Git 子项目 workspace |
| `docs-seed branches sync` | 拉取远端 refs 并重建分支谱系 |
| `docs-seed learn current` | 学习当前匹配主分支的代码事实 |
| `docs-seed learn history` | 结合提交历史学习当前分支 |
| `docs-seed learn evolution` | 从根主分支第一个提交开始，按提交顺序学习当前链路的业务演进 |
| `docs-seed generate docs` | 从已学习事实生成当前分支链文档 |
| `docs-seed sync` | 同步谱系、学习当前链路并生成文档 |
| `docs-seed sync --evolution` | 同步谱系、逐提交学习业务演进并生成文档 |
| `docs-seed sync --evolution --direct-write` | 让 Agent 直接写 Markdown 文档，主进程只负责 Git 范围和进度 |
| `docs-seed sync --branch <name>` | 为指定匹配主分支生成完整阅读链 |
| `docs-seed workspace add` | 扫描并初始化第一层独立 Git 子项目 |
| `docs-seed preview branches` | 只读预览计算得到的分支谱系 |
| `docs-seed preview files` | 预览当前链路的 fork point 和 tip |

`sync` 和学习命令只分析已提交代码。工作树有未提交修改时会给出警告，不会把这些
修改混入文档，也不会切换用户当前分支。非当前分支通过 `git archive` 创建临时只读
快照供 Agent 分析。

## Workspace

```bash
docs-seed init --workspace
docs-seed workspace add
docs-seed sync
```

workspace 根目录只保存子项目索引。每个独立 Git 子项目拥有自己的 `.docs-seed.yml`、
分支谱系、学习状态和文档，避免不同仓库的主分支关系相互污染。

## 本地状态

```text
.docs-seed/
├── branch-graph.json       # 可版本化的分支谱系
├── docs/                   # 可版本化的人类阅读文档
├── state/                  # 本地学习事实，默认忽略
└── memory/                 # Agent 运行信息，默认忽略
```

生成文档末尾保留文件级证据和 commit 范围，用于人工核验；不会写代码调用步骤。每个
分支目录包含业务逻辑、数据流转和 ADR 三类结果文档。使用 `--evolution` 时还会生成
`commit-evolution.md`，按 Git 提交顺序列出每次提交提取出的业务演进事实，供人工或
后续 LLM 复核最终汇总如何形成。

## 提交演进模式

`docs-seed sync --evolution --branch <target>` 会先计算目标主分支的阅读链，例如
`A → B → C`，再按链路依次学习：

- 对根主分支 A，从 Git 可达历史的第一个提交开始，按 `git log --reverse` 顺序读取
  每个 commit 的 message、变更文件和 diff。
- 对增量主分支 B/C，只读取其相对父主分支 fork point 之后的 commit。
- 每个 commit 单独生成一个缓存事实，落在 `.docs-seed/state/commits/<branch>/`。
- 分支级 `business-logic.md`、`data-flow.md`、`adr.md` 由这些 commit 事实去重汇总。
- `commit-evolution.md` 保留逐 commit 演进链，便于其他 LLM 继续审查、补充或重写总结。

如果本地 CLI Agent 的 JSON 输出不稳定，可以使用：

```bash
docs-seed sync --evolution --direct-write --branch <target>
```

direct-write 模式下，Docs Seed 不再解析 Agent 返回的 JSON。它把分支链路、commit
message、变更文件和 diff 写入 `.docs-seed/tmp/direct-write/` 的单提交材料文件，
再指挥 Claude/Codex 读取该材料文件并直接写
`business-logic.md`、`data-flow.md`、`adr.md` 和 `commit-evolution.md`。主进程只
负责 Git 范围、进度回显、结果校验和根索引生成。每个有效提交处理后，
`commit-evolution.md` 必须包含该 commit 的完整 hash 或短 hash；否则本次同步会失败，
避免 Agent 没有真正更新最终文档却继续向后处理。

direct-write 的恢复存档保存在最终文档根目录：

```text
<docs.output>/docs-seed-checkpoint.json
```

每次运行都会重新计算当前阅读链路和各分支段的提交集合，再用存档点和最终
`commit-evolution.md` 共同判断是否跳过。只有“存档点已记录且最终文档也包含该 commit
hash”的提交才会跳过；如果最终文档已有记录但存档点缺失，Docs Seed 会补写存档点。

调试 Agent 写入行为时可以限制处理数量：

```bash
docs-seed sync --evolution --direct-write --branch <target> --limit-commits 1
```

## 开发验证

```bash
docker run --rm \
  -v "$PWD":/workspace \
  -w /workspace \
  golang:1.25.6-bookworm \
  sh -c '/usr/local/go/bin/gofmt -w . &&
    /usr/local/go/bin/go mod tidy &&
    /usr/local/go/bin/go vet ./... &&
    /usr/local/go/bin/go test ./... &&
    /usr/local/go/bin/go build -o dist/docs-seed ./cmd/docs-seed'
```

本项目包含从 Skills Seed 工作流中派生的设计思路和少量结构性实现，继续遵循 MIT
许可证，并在 `LICENSE` 中保留归属。

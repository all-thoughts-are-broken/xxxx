# fonts/

渲染命令（`render_album_detail_image` / `render_album_comment_image`）需要的中文字体。

| 文件 | 字重 | 用途 |
|---|---|---|
| `NotoSansSC-Regular.ttf` | Regular | 正文 |
| `Noto-Sans-SC-Bold-2.ttf` | Bold | 标题 |

## 为什么字体要放在仓库里

这两个命令要画中文（作品名、作者、标签、评论正文），必须有一个含中文字形的字体。
系统字体在不同平台上名字和路径都不一样，靠"找系统里的中文字体"在三个平台上都会
失效，所以默认值指向仓库内的相对路径 `fonts/`。

不需要渲染功能的话可以删掉这两个文件（约 26 MB）；缺字体时渲染命令会返回
`internal` 错误并写明期望的路径，其余命令不受影响。相关测试在字体缺失时会自动
skip，所以 `go test ./...` 依然全绿。

字体路径可在配置里改（`font_regular` / `font_bold`），也可以用 `config_set` 在运行时
覆盖。解析顺序是「配置里写的 → 可执行文件同级 → 当前工作目录」。

## 许可

两个文件都是 **Noto Sans SC**，采用
[SIL Open Font License 1.1](LICENSE-OFL.txt)（OFL-1.1），全文见同目录的
`LICENSE-OFL.txt`。

OFL 允许自由使用、修改、再分发，包括随商业软件一起分发；要求是：

- 保留本目录下的版权与许可声明（`LICENSE-OFL.txt`）；
- 若修改了字体文件本身，不得继续使用 "Noto" 这一保留字体名称（Reserved Font Name）。

本项目**未修改**字体文件，仅原样分发。

上游来源：

- Noto Sans SC — <https://fonts.google.com/noto/specimen/Noto+Sans+SC>
- notofonts/noto-cjk — <https://github.com/notofonts/noto-cjk>

## 声明

字体版权归 Google / Adobe 及 Noto 项目贡献者所有，不与本项目的 MIT 许可混同。
本项目的 MIT 许可（见仓库根 `LICENSE`）只覆盖本项目的源代码。

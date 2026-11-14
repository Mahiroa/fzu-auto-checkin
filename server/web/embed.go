// Package web 只做一件事：把 web/static 下的前端产物编进二进制。
//
// 为什么单独占一个目录而不是放进 internal/server：go:embed 的路径必须相对于
// **指令所在文件**的目录，且不能引用父目录（`../static` 是编译错误）。静态资源
// 定在 web/static（§4 的布局），因此 embed 指令只能写在这一层，由 server 包
// fs.Sub 到 "static" 再托管。
//
// 前端是零构建的原生 HTML/JS/CSS（§13），所以这里不需要任何打包步骤：
// 改完 static 下的文件重新 go build 即可。
package web

import "embed"

// FS 是整棵内嵌静态资源树，根下只有一个 "static" 目录。
// 导出的名字只叫 FS 而不是 StaticFS：调用方需要哪个子目录由它自己 fs.Sub。
//
//go:embed static
var FS embed.FS

// StaticDir 是 FS 内静态资源所在的子目录名，server 包用它做 fs.Sub。
const StaticDir = "static"

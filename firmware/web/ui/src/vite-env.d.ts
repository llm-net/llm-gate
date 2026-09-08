/// <reference types="vite/client" />

/**
 * 只为拿到 Vite 的资源模块声明（`*.css` 副作用 import、`*.svg?raw` 等）。
 * 产品面不使用 VITE_* 环境变量：页面同源访问板上 gatewayd，没有可配置的地址。
 */

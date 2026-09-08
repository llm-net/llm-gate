// 入口只做两件事：先把当前语言的文案目录取回来（lib/i18n.tsx），再动态 import
// 真正的装配模块 bootstrap.tsx。顺序不能反——业务模块在加载期就会调 t()，
// 目录晚到一步，那些模块级常量就永远是中文。本文件不得静态 import 任何业务模块。

import "@/styles/globals.css";
import { initI18n } from "@/lib/i18n";

await initI18n();
const { bootstrap } = await import("@/bootstrap");
bootstrap();

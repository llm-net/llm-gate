// 产品品牌常量。硬件型号由服务端按本机 boardinfo 型号档案识别，固件版本由
// buildinfo 给出；二者都不是前端常量：登录页走免会话的 firmwareVersion()，
// 登录后的顶栏与状态页搭各自已有读数返回。不要在这里写死某块开发板型号。
//
// 产品名是 LLM Gate（独立项目，无公司落款）。登录大字标与读数条都用这一串
// 大小写，不全大写、不跟公司名。
export const productName = "LLM Gate";

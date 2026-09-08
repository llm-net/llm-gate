// 金额换算：线上恒为整数微元，元只是录入与显示的皮肤。逐字照搬自管理台
// web/admin/src/api.ts 的同名一节——金额禁止浮点这条硬约束在前端一样成立。
// ---- 金额换算（iteration-9）：线上恒为整数微元，元只是录入与显示的皮肤 ----

// MicroPerYuan 与服务端 usage.MicroPerYuan 同值（1 元 = 10⁶ 微元）。所有换算
// 走本节两个函数，别在页面里另写 /1e6——金额禁止浮点这条硬约束在前端一样成立。
export const MicroPerYuan = 1_000_000;

// YuanInputMaxDecimals 是录入精度上限：3 位小数（= 1000 微元）。再细的位数
// 没有业务含义，却会把「看起来一样、存进去不一样」的两个值混在一起。
export const YuanInputMaxDecimals = 3;

// MaxPricingMicro 与服务端 store.MaxPricingMicro 同值（10¹² 微元 = 100 万元 /
// 计价单位）：超出即 400，前端先拦一道给人话。预算录入沿用同一上限——这台
// 设备上不存在百万元量级的日/月预算，拦下手滑多打的那个零比放行划算。
export const MaxPricingMicro = 1_000_000_000_000;

// parseYuan 把「元」录入折成整数微元；null = 格式不合法（非数字 / 负数 /
// 小数超过 3 位 / 超出上限）。空串的含义随场景不同（预算空 = 不限，价格空 =
// 这一档不配），由调用方自己判，本函数一律视为不合法。
//
// 刻意走字符串拆分而不是 Number(text) * 1e6：后者在 0.07 这类值上得到
// 70000.000000000015，这一次取整回来对，换个值就不一定。
export function parseYuan(text: string): number | null {
  const s = text.trim();
  if (!/^\d+(\.\d{1,3})?$/.test(s)) return null;
  const dot = s.indexOf(".");
  const frac = dot < 0 ? "" : s.slice(dot + 1);
  const whole = Number(dot < 0 ? s : s.slice(0, dot));
  const micro = whole * MicroPerYuan + Number(`${frac}000`.slice(0, 3)) * 1000;
  if (!Number.isSafeInteger(micro) || micro > MaxPricingMicro) return null;
  return micro;
}

// fmtYuan 把整数微元渲染成「元」读数：digits=2 是常规金额，4 用于明细（一次
// 请求的金额常在分以下）。这里是**渲染**，除法落在 JS number 上——`micro` 恒 ≤
// MaxPricingMicro=10¹² ≪ 2⁵³，除数是 10⁴/10²，结果精确；金额的存储与运算一律整数
// 微元（`parseYuan` 按字符串切，不做 `Number(text)*1e6`），别把这里的写法搬去算钱。
export function fmtYuan(micro: number, digits: 2 | 4 = 2): string {
  const scale = digits === 2 ? 100 : 10_000;
  const n = Math.round(micro / (MicroPerYuan / scale)); // 折到 10^-digits 元的整数单位
  const sign = n < 0 ? "-" : "";
  const abs = Math.abs(n);
  const whole = String(Math.floor(abs / scale)).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return `${sign}${whole}.${String(abs % scale).padStart(digits, "0")}`;
}

// fmtMoney 是界面上金额的统一读数：两位小数，但**不足一分的金额改用四位**。
// 这台设备上单次调用常在厘以下（一次 42 token 的对话是 0.0002 元），一律两位
// 会把「有花钱」显示成「¥0.00」——屏幕上最容易被当成 bug 的一种真话。
export function fmtMoney(micro: number): string {
  const digits = micro !== 0 && Math.abs(micro) < MicroPerYuan / 100 ? 4 : 2;
  return `¥${fmtYuan(micro, digits)}`;
}

// yuanText 是录入框的回填值：整数微元 → 最简小数（不补零，便于直接改）。
export function yuanText(micro: number): string {
  const sign = micro < 0 ? "-" : "";
  const abs = Math.abs(micro);
  const frac = String(abs % MicroPerYuan).padStart(6, "0").replace(/0+$/, "");
  return `${sign}${Math.floor(abs / MicroPerYuan)}${frac === "" ? "" : `.${frac}`}`;
}

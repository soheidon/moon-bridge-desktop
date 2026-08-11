import { test, expect } from "vitest";
import { formatCurrency } from "./shared";

test("formatCurrency follows the UI locale, not the host default", () => {
  // CNY is the app's billing currency. Symbol is U+00A5 in en-US/zh-CN and
  // U+FFE5 in ja-JP; omitting the locale would fall back to the host default.
  expect(formatCurrency(0.42, "CNY", "en-US")).toBe("¥0.42");
  expect(formatCurrency(1105.26, "CNY", "zh-CN")).toBe("¥1,105.26");
  expect(formatCurrency(0.42, "CNY", "ja-JP")).toBe("￥0.42");
  // Documented contract: JPY follows the same locale rule.
  expect(formatCurrency(1000, "JPY", "en-US")).toBe("¥1,000.00");
  expect(formatCurrency(1000, "JPY", "ja-JP")).toBe("￥1,000.00");
});

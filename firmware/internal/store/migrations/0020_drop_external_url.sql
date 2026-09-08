-- 0020_drop_external_url: 设备不保存客户网络边界上的转发地址。
-- 路由器、反向代理或网关直接把任意域名转发到设备监听器即可；设备不需要
-- 记录或回显这条客户自管链路。
DELETE FROM settings
WHERE key = 'external_url';

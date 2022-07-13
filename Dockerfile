# 打镜像
FROM registry.baidubce.com/cce-service-pro/cce-base:v1.0.0

# 设置时区
ENV TZ=Asia/Shanghai

COPY  telegraf /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/telegraf"]

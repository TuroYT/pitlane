# Small image for the e2e docker scenarios. The sleep makes a cache hit
# visible: a warm build skips it.
FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache curl
RUN sleep 8 && uname -m > /built-arch
COPY hello.txt /hello.txt
CMD ["sh", "-c", "cat /hello.txt; echo arch=$(cat /built-arch)"]

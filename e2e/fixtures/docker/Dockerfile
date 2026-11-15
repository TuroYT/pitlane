# Small image for the e2e docker scenarios. The sleep makes a cache hit
# visible: a warm build skips it.
FROM alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
RUN apk add --no-cache curl
RUN sleep 8 && uname -m > /built-arch
COPY hello.txt /hello.txt
CMD ["sh", "-c", "cat /hello.txt; echo arch=$(cat /built-arch)"]

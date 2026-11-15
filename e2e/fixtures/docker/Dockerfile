# Small image for the e2e docker scenarios. The sleep makes a cache hit
# visible: a warm build skips it.
FROM alpine:3.22
RUN apk add --no-cache curl
RUN sleep 8 && uname -m > /built-arch
COPY hello.txt /hello.txt
CMD ["sh", "-c", "cat /hello.txt; echo arch=$(cat /built-arch)"]

# syntax=docker/dockerfile:1
FROM node:22-alpine AS web
WORKDIR /src/web
RUN corepack enable
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /pitlane ./cmd/pitlane

FROM gcr.io/distroless/static-debian12
COPY --from=build /pitlane /pitlane
ENV PITLANE_DATA=/data PITLANE_LISTEN=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/pitlane"]

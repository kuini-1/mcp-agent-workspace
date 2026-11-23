# --- dashboard ---
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- server ---
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agentws ./cmd/agentws

# --- runtime ---
# The agent runs shell commands, so the image ships a small toolbox and runs
# as an unprivileged user. Mount your project at /workspace.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates git bash curl jq \
 && adduser -D -u 10001 agent \
 && mkdir -p /data /workspace \
 && chown agent:agent /data /workspace
COPY --from=build /out/agentws /usr/local/bin/agentws
USER agent
ENV AGENTWS_DATA=/data \
    AGENTWS_WORKSPACE=/workspace \
    AGENTWS_ADDR=0.0.0.0:8080
VOLUME ["/data", "/workspace"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["agentws"]
CMD ["serve"]

# The scheduler talks to the Docker Engine API over a mounted socket; it needs
# no Docker CLI inside the image. Run it with the socket and its group, e.g.
#
#   docker run -d --name renovate-scheduler \
#     --group-add "$(stat -c %g /var/run/docker.sock)" \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v renovate-scheduler-data:/data \
#     -e BASE_URL=https://renovate.example.com \
#     -e FORGEJO_URL=https://git.example.com \
#     -e OAUTH_CLIENT_ID=... -e OAUTH_CLIENT_SECRET=... \
#     -e RENOVATE_TOKEN=... -e ALLOWED_USERS=alice \
#     -p 8080:8080 renovatebot-scheduler

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/renovatebot-scheduler .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 app \
    && mkdir -p /data && chown app:app /data
COPY --from=build /out/renovatebot-scheduler /usr/local/bin/renovatebot-scheduler
USER app
WORKDIR /data
ENV ADDR=:8080 DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["renovatebot-scheduler"]

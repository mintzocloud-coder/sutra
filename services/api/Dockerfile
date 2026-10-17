# SPDX-License-Identifier: AGPL-3.0-or-later
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /api ./cmd/api

FROM alpine:3.20
COPY --from=build /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]

# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/al-maisan/aiproxy/internal/version.Version=${VERSION} \
      -X github.com/al-maisan/aiproxy/internal/version.Commit=${COMMIT} \
      -X github.com/al-maisan/aiproxy/internal/version.Date=${DATE}" \
    -o /out/aiproxy ./cmd/aiproxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/aiproxy /usr/local/bin/aiproxy
EXPOSE 8787
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/aiproxy"]

# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /fedan ./cmd/fedan
RUN mkdir /data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /fedan /fedan
# distroless has no shell to mkdir/chown, so /data comes from the build stage
COPY --from=build --chown=65532:65532 /data /data
ENV FEDAN_ADDR=:8080 FEDAN_DB_PATH=/data/fedan.db
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/fedan", "healthcheck"]
ENTRYPOINT ["/fedan"]

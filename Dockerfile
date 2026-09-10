# Build estático: o binário final roda em distroless, que não tem libc para
# linkar em tempo de execução.
FROM golang:1.24 AS build
WORKDIR /src

# go.mod e go.sum primeiro: enquanto as dependências não mudam, a camada de
# download fica em cache e o build só recompila o código.
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/manager ./cmd/manager

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=build /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

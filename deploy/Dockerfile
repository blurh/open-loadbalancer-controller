# controller 端镜像：编译 ./cmd/controller 这个 main，产出静态二进制后放进 distroless。
FROM golang:1.25.5 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/openlb-controller ./cmd/controller/

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/openlb-controller /openlb-controller
USER nonroot:nonroot
ENTRYPOINT ["/openlb-controller"]

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY processorapi ./processorapi
COPY examples/processors/reference ./reference
RUN CGO_ENABLED=0 go build -trimpath -o /reference ./reference
FROM scratch
COPY --from=build /reference /reference
USER 65532:65532
ENTRYPOINT ["/reference"]

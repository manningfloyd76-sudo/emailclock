FROM golang:1.24 AS build
WORKDIR /src
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tick .

FROM gcr.io/distroless/static
COPY --from=build /tick /tick
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/tick"]

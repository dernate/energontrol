module github.com/dernate/energontrol/v2

// go: the minimum a consumer of this module needs.
// toolchain: what building *this* module uses, which may be newer — the go
// documentation names exactly this case for a module that is a dependency of
// others. Bump it with: go get toolchain@go1.26.8
go 1.26

toolchain go1.26.7

require (
	github.com/dernate/gopcxmlda v1.2.2
	github.com/joho/godotenv v1.5.1
)

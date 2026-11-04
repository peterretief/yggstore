# codemap

One page that maps a Go module: every package, what it builds on within
the module, and the functions in each of its files, searchable, and for a
module on GitHub linked to each function's line. Tests are left out.

```sh
cd ~/src/your-module
go run github.com/peterretief/yggstore/tools/codemap@main -o code-map.html
```

`-root DIR` maps another folder. The page is one HTML file with nothing to
install or serve: open it in a browser, or put it on any website.

yggstore's own: [get.withcare.co.za/code-map](https://get.withcare.co.za/code-map/).

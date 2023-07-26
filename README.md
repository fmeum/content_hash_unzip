## Installation

```bash
$ go install github.com/fmeum/content_hash_unzip@latest
```

## Usage

```bash
# Print the content hash of a ZIP file and check that its contents satisfy all
# restrictions.
$ content_hash_unzip some.zip
h1:F5f5VUGvRisY5bwl1HWQFYl5StpfzKiA4I3lJxgmc7c=


# Extract the ZIP into some/dir if the content hash matches and all restrictions
# are satisfied. Otherwise fail with a non-zero exit code (some files may end up
# being extracted in this case).
$ content_hash_unzip some.zip h1:F5f5VUGvRisY5bwl1HWQFYl5StpfzKiA4I3lJxgmc7c=

# Extract the ZIP, stripping the given path prefix.
$ content_hash_unzip some.zip h1:F5f5VUGvRisY5bwl1HWQFYl5StpfzKiA4I3lJxgmc7c= my_prefix
```

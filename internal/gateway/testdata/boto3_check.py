"""Exercise the gateway with the AWS SDK for Python (boto3).

Run by TestBoto3 when YGGSTORE_TEST_PYTHON points at a Python with boto3.
Newer boto3 sends checksums in aws-chunked trailers, which this covers.
"""
import io
import os
import sys
import urllib.error
import urllib.request

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url=os.environ["ENDPOINT"],
    aws_access_key_id=os.environ["ACCESS_KEY"],
    aws_secret_access_key=os.environ["SECRET_KEY"],
    region_name="us-east-1",
    config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 1}),
)


def check(cond, what):
    if not cond:
        print("FAILED:", what)
        sys.exit(1)


s3.create_bucket(Bucket="boto")
s3.put_object(Bucket="boto", Key="hello.txt", Body=b"hello world", ContentType="text/plain",
              Metadata={"colour": "blue"})
h = s3.head_object(Bucket="boto", Key="hello.txt")
check(h["ContentLength"] == 11 and h["Metadata"] == {"colour": "blue"} and h["ContentType"] == "text/plain", "head")

big = os.urandom(12 * 1024 * 1024 + 99)
s3.upload_fileobj(io.BytesIO(big), "boto", "dir/big.bin",
                  Config=TransferConfig(multipart_threshold=5 * 1024 * 1024, multipart_chunksize=5 * 1024 * 1024))
got = s3.get_object(Bucket="boto", Key="dir/big.bin")["Body"].read()
check(got == big, "multipart round trip")
part = s3.get_object(Bucket="boto", Key="dir/big.bin", Range="bytes=5242870-5242889")["Body"].read()
check(part == big[5242870:5242890], "range")

s3.copy_object(Bucket="boto", Key="copy.txt", CopySource={"Bucket": "boto", "Key": "hello.txt"})
check(s3.get_object(Bucket="boto", Key="copy.txt")["Body"].read() == b"hello world", "copy")

for i in range(5):
    s3.put_object(Bucket="boto", Key=f"many/{i}", Body=b"x")
keys = []
for page in s3.get_paginator("list_objects_v2").paginate(Bucket="boto", Prefix="many/", PaginationConfig={"PageSize": 2}):
    keys += [o["Key"] for o in page.get("Contents", [])]
check(keys == [f"many/{i}" for i in range(5)], "paginated listing: %s" % keys)
top = s3.list_objects_v2(Bucket="boto", Delimiter="/")
check([p["Prefix"] for p in top["CommonPrefixes"]] == ["dir/", "many/"], "common prefixes")

url = s3.generate_presigned_url("get_object", Params={"Bucket": "boto", "Key": "hello.txt"}, ExpiresIn=300)
check(urllib.request.urlopen(url).read() == b"hello world", "presigned link (v2)")
v4 = boto3.client("s3", endpoint_url=os.environ["ENDPOINT"], aws_access_key_id=os.environ["ACCESS_KEY"],
                  aws_secret_access_key=os.environ["SECRET_KEY"], region_name="us-east-1",
                  config=Config(signature_version="s3v4", s3={"addressing_style": "path"}))
url = v4.generate_presigned_url("get_object", Params={"Bucket": "boto", "Key": "hello.txt"}, ExpiresIn=300)
check(urllib.request.urlopen(url).read() == b"hello world", "presigned link (v4)")
try:
    urllib.request.urlopen(url.replace("hello.txt", "copy.txt"))
    check(False, "a link opened another object")
except urllib.error.HTTPError as e:
    check(e.code == 403, "tampered link: %d" % e.code)

all_keys = [o["Key"] for o in s3.list_objects_v2(Bucket="boto")["Contents"]]
s3.delete_objects(Bucket="boto", Delete={"Objects": [{"Key": k} for k in all_keys]})
s3.delete_bucket(Bucket="boto")
check("boto" not in [b["Name"] for b in s3.list_buckets()["Buckets"]], "bucket deleted")
print("boto3 OK")

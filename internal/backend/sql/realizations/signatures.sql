select
  "signature_public_keys"."format" as "format",
  "signature_public_keys"."public_key" as "public_key",
  "signatures"."signature" as "signature"
from
  "signatures"
  join "signature_public_keys" on "signature_public_keys"."id" = "signatures"."public_key_id"
where
  "signatures"."drv_hash" = (select "id" from "drv_hashes" where ("algorithm", "bits") = (:drv_hash_algorithm, :drv_hash_bits)) and
  "signatures"."output_name" = :output_name and
  "signatures"."output_path" = (select "id" from "paths" where "path" = :output_path)
order by 1, 2;

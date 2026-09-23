select
  "paths"."path" as "path",
  "drv_hashes"."algorithm" as "drv_hash_algorithm",
  "drv_hashes"."bits" as "drv_hash_bits",
  "reference_classes"."reference_output_name" as "output_name"
from
  "reference_classes"
  join "paths" on "paths"."id" = "reference_classes"."reference"
  join "drv_hashes" on "drv_hashes"."id" = "reference_classes"."reference_drv_hash"
where
  "reference_classes"."referrer_drv_hash" = (select "id" from "drv_hashes" where ("algorithm", "bits") = (:drv_hash_algorithm, :drv_hash_bits)) and
  "reference_classes"."referrer_output_name" = :output_name and
  "reference_classes"."referrer" = (select "id" from "paths" where "path" = :output_path)
order by 1, 2, 3, 4;

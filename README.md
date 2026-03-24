# HandleDuplicateDesign

Tool Go để quét ảnh design trong một thư mục lớn (đệ quy), phát hiện ảnh trùng/na ná nhau dù khác kích thước, sau đó giữ lại ảnh có độ phân giải lớn nhất và xóa phần còn lại.

## Pipeline xử lý

```text
INPUT FOLDER
    ↓
Scan images
    ↓
Extract metadata (width, height, pixels)
    ↓
Normalize image
    ↓
Generate perceptual hash (pHash)
    ↓
Build LSH buckets
    ↓
BK-tree search trong từng bucket
    ↓
Hamming distance filter
    ↓
Group similar images (Union-Find)
    ↓
For each group:
    Select largest resolution image
    Delete other images
    ↓
Write log
    ↓
DONE
```

## Cách hoạt động ngắn gọn

- Mỗi ảnh được decode và resize về kích thước chuẩn để giảm nhiễu do khác kích thước gốc.
- Tạo `pHash` 64-bit cho từng ảnh.
- Dùng LSH để chia ảnh vào các bucket theo từng band bit giống nhau.
- Trong từng bucket, dùng BK-tree để tìm nhanh các ảnh có khoảng cách Hamming trong ngưỡng.
- Các cặp đạt ngưỡng được nối nhóm bằng Union-Find.
- Mỗi nhóm chỉ giữ ảnh có `width * height` lớn nhất, ảnh còn lại sẽ bị xóa (hoặc chỉ log nếu bật `-dry-run`).

## Yêu cầu

- Go (khuyến nghị bản mới).
- Hệ điều hành: Windows/macOS/Linux.

## Build

```bash
go mod tidy
go build -o dedupe.exe .
```

## Chạy chương trình

```bash
./dedupe.exe -root "D:\Designs" -distance 10 -lsh-bits 2 -dry-run -log "dedupe_design.log"
```

Nếu chạy PowerShell:

```powershell
.\dedupe.exe -root "D:\Designs" -distance 10 -lsh-bits 2 -dry-run -log "dedupe_design.log"
```

Chạy nhanh chỉ với thư mục gốc (dùng toàn bộ giá trị mặc định):

```powershell
.\dedupe.exe "D:\Designs"
```

## Tham số CLI

- `-root`  
  Thư mục gốc chứa ảnh (quét đệ quy). Mặc định: `"."`

- `-distance`  
  Ngưỡng Hamming distance cho pHash. Mặc định: `10`  
  Giá trị nhỏ hơn -> khắt khe hơn (ít false positive hơn, nhưng có thể sót).

- `-lsh-bits`  
  Số bit cho mỗi band của LSH. Mặc định: `2`  
  Chỉ nhận các giá trị chia hết 64: `1, 2, 4, 8, 16`.

- `-dry-run`  
  Chỉ ghi log, không xóa file. Mặc định: `false`

- `-log`  
  Đường dẫn file log output. Mặc định: `dedupe_design.log`

## Gợi ý cấu hình thực tế

- Luôn chạy thử với `-dry-run` trước để kiểm tra nhóm ảnh.
- Bộ tham số an toàn để bắt đầu:
  - `-distance 8` hoặc `-distance 10`
  - `-lsh-bits 2`
- Nếu muốn khắt khe hơn (giảm xóa nhầm), giảm `-distance` xuống `6-8`.
- Nếu tăng `-distance`, nên giảm `-lsh-bits` để tăng số band và giữ độ bao phủ tìm kiếm.

## Ý nghĩa log

Log sẽ ghi:

- Cấu hình chạy (`root`, `max_hamming`, `lsh_bits_per_band`, `lsh_bands`, `dry_run`)
- Mỗi nhóm ảnh tương tự:
  - `KEEP`: ảnh được giữ
  - `DEL`: ảnh bị xóa (hoặc sẽ xóa trong dry-run)
- Dòng tổng kết cuối cùng.

## Lưu ý quan trọng

- Thuật toán là so khớp gần đúng dựa trên perceptual hash, không phải so khớp pixel tuyệt đối.
- Nên backup dữ liệu hoặc dùng `-dry-run` nhiều lần trước khi chạy xóa thật.
- Với dataset rất lớn, hiệu năng cải thiện đáng kể nhờ LSH + BK-tree so với so sánh toàn bộ cặp ảnh.

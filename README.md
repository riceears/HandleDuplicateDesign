# HandleDuplicateDesign

Tool Go để quét ảnh design trong một thư mục lớn (đệ quy), phát hiện ảnh trùng/na ná nhau dù khác kích thước, sau đó giữ lại ảnh có độ phân giải lớn nhất và xóa phần còn lại.
Chương trình dùng kiến trúc **2 phase** để phù hợp tập dữ liệu lớn (100k+ ảnh).

## Pipeline xử lý

```text
PHASE 1 (batch local dedupe)
    - Quét toàn bộ ảnh
    - Chia theo lô (mặc định 5000)
    - Mỗi lô: pHash -> LSH -> BK-tree -> Group -> Keep largest -> Delete
    - Xóa thư mục rỗng
    ↓
PHASE 2 (global merge)
    - Quét lại ảnh còn sống
    - Chạy dedupe toàn cục để bắt duplicate cross-batch
    - Xóa thư mục rỗng
    ↓
Write final log
    ↓
DONE
```

## Cách hoạt động ngắn gọn

- Phase 1 giúp giảm dữ liệu nhanh bằng cách xử lý cục bộ theo lô.
- Phase 2 dedupe toàn cục trên ảnh còn lại để không bỏ sót duplicate nằm khác batch.
- Sau mỗi phase, chương trình dọn thư mục rỗng từ dưới lên.

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
.\dedupe.exe -root "D:\Designs" -distance 10 -lsh-bits 2 -batch-size 5000 -dry-run -log "dedupe_design.log"
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

- `-batch-size`
  Số ảnh mỗi lô trong Phase 1. Mặc định: `5000`

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
- Log theo từng batch của Phase 1, log Phase 2 global.
- Mỗi nhóm ảnh tương tự:
  - `KEEP`: ảnh được giữ
  - `DEL`: ảnh bị xóa (hoặc sẽ xóa trong dry-run)
- Dòng tổng kết từng phase + tổng kết cuối.

## Lưu ý quan trọng

- Thuật toán là so khớp gần đúng dựa trên perceptual hash, không phải so khớp pixel tuyệt đối.
- Nên backup dữ liệu hoặc dùng `-dry-run` nhiều lần trước khi chạy xóa thật.
- Với dataset rất lớn, hiệu năng cải thiện đáng kể nhờ LSH + BK-tree so với so sánh toàn bộ cặp ảnh.

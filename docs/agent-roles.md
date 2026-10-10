# BuoyUp — Vai trò của 4 AI agent

> Tài liệu mô tả vai trò tổng quan (overall role) của từng agent.
> Căn cứ: "BuoyUp toàn cảnh mô hình" (28.09.2026) và component FISCO BCOS đã build.

---

## 0. Nguyên tắc chung (áp dụng cho cả 4 agent)

1. **Agent không tự tính con số.** Hạn mức, lãi suất, tỷ lệ trích nợ, điểm rủi ro đều do Machine Learning và policy engine (code cố định) tính. Agent chỉ đọc kết quả, phán đoán trên dữ liệu phi cấu trúc và giải thích.
2. **Agent không phải bên phê duyệt cuối cùng.**
   - Tầng 1 (khoản nhỏ): policy engine duyệt tự động theo luật cố định.
   - Tầng 2 vượt ngưỡng, hoặc khi chạm 1 trong 6 điều kiện kích hoạt: cán bộ WeBank ký.
3. **Bộ điều phối (orchestrator) là code cố định, không phải AI.** Nó gọi agent theo thứ tự, áp luật chuyển con người, và là bên duy nhất ghi lên FISCO. Agent không giữ khóa ký blockchain.
4. **Agent không đọc tài liệu gốc.** Tài liệu người bán nộp (hợp đồng, kế hoạch, sao kê) đi qua bộ trích xuất cô lập, thành các trường cố định. Agent chỉ nhận các trường đó, để chống prompt injection.
5. **Mọi đầu vào/đầu ra của agent đều được lưu vết.** Hash được anchor lên FISCO (`POST /v1/audit/anchors`, kind `agent_io`).
6. **Không vượt trần ML.** Dù agent bị thao túng, hợp đồng trên chain vẫn chặn: lãi không vượt `maxRateBps`, tỷ lệ trích không vượt trần, giải ngân đúng 1 lần đúng số tiền.

---

## 1. Luồng tổng

```
Người bán nộp hồ sơ
      │
      ▼
[Agent 1 — Tiếp nhận & Xác minh] ──► FISCO: anchor hồ sơ + kết quả xác minh
      │ (đủ & khớp)              └─► từ chối sớm nếu fail luật cứng (do policy engine quyết)
      ▼
[ML core + Policy engine]  ── tính PD, nhóm KH, hạn mức TRẦN, lãi, tỷ lệ trích (code, không phải agent)
      │
      ▼
[Agent 2 — Thẩm định & Tư vấn] ──► đề xuất số tiền ≤ trần + giải thích cho người bán
      │
      ▼
[Cổng chuyển con người] ── vượt ngưỡng / bất thường ──► Cán bộ WeBank ký
      │
      ▼
FISCO: POST /v1/loans  (+ /officer-decision nếu cần)
      │  người bán bấm chấp nhận
      ▼
[Agent 3 — Giải ngân, Thu hồi & Giám sát] ──► FISCO: /disburse, /repayments, /alerts...
      │
      ▼
[Agent 4 — Hậu kiểm & Cải tiến] ── đọc lịch sử trên FISCO + kết quả trả nợ ──► báo cáo đề xuất cho con người
```

---

## 2. Agent 1 — Tiếp nhận & Xác minh

**Tương đương:** nhân viên quan hệ khách hàng + bộ phận xác minh hồ sơ.

**Việc chính**
- Hướng dẫn người bán nộp hồ sơ, kiểm tra hồ sơ đủ và khớp nhau. Còn thiếu thì yêu cầu bổ sung.
- Gửi yêu cầu ủy quyền (tra cứu tín dụng, tra cứu dòng tiền, trích nợ, chia sẻ dữ liệu cửa hàng).
- Xác minh cửa hàng: có tồn tại thật không, tuổi cửa hàng, điểm cửa hàng, có vi phạm không.
- Lấy báo cáo / điểm tín dụng qua **kênh có giấy phép**: Ngân hàng Nhân dân, 百行征信, 朴道征信, và **钱塘征信** (có vốn Ant Group, được cấp phép 2024). Dữ liệu của Ant Group đi vào qua kênh 钱塘征信 này, không lấy thẳng từ nền tảng.
- Lấy **kết quả** từ FATE: điểm rủi ro kết hợp, tín hiệu bất thường (buff đơn, đơn không khớp vận đơn).
- Đối chiếu tự động: doanh số kê khai ↔ vận đơn, tiền nền tảng ↔ dòng tiền thực tế về tài khoản.
- Tổng hợp thành hồ sơ đã xác minh, rồi chuyển sang ML và Agent 2.

**Ghi lên FISCO** (qua orchestrator)
- Hash bộ hồ sơ đã trích xuất: kind `application`.
- Hash kết quả xác minh và các cờ bất thường: kind `agent_io`.

**Không được làm**
- Không tự từ chối dựa trên cảm tính. Từ chối sớm chỉ khi **luật cứng** của policy engine fail, ví dụ cửa hàng không tồn tại, đang nợ xấu, hay thuộc nhóm 4.
- Không đánh giá rủi ro, không hứa hạn mức với người bán.

**Chuyển con người khi:** có tín hiệu gian lận, hoặc người bán yêu cầu.

---

## 3. Agent 2 — Thẩm định & Tư vấn khoản vay

**Tương đương:** cán bộ tín dụng thứ nhất + chatbot tư vấn.

**Việc chính**
- Đọc các trường đã trích xuất (kế hoạch nhập hàng, hợp đồng nhà cung cấp, kế hoạch bán) và đối chiếu với dự báo của ML.
- Phân biệt doanh số giảm do mùa vụ hay do suy yếu thật.
- **Đề xuất** số tiền **≤ hạn mức trần do ML tính**, kèm covenants, kỳ hạn, ân hạn. Viết tờ trình.
- Thông báo cho người bán:
  - được duyệt bao nhiêu, tối đa bao nhiêu
  - lãi suất năm và tổng chi phí
  - lịch trả nợ
- Giải thích theo Điều 24 Luật BVTTCN:
  - vì sao không được vay cao hơn, ở mức **yếu tố** (ví dụ "tỷ lệ hoàn hàng cao", "lịch sử mùa vụ ngắn"), không tiết lộ ngưỡng chính xác
  - cần bổ sung gì để được hạn mức cao hơn, và lộ trình cải thiện
  - người bán có quyền yêu cầu con người xem xét lại

**Ghi lên FISCO**
- Hash tờ trình: kind `credit_memo`.
- Hash lời giải thích gửi người bán: kind `explanation`.
- Gói quyết định cuối cùng (ML + tờ trình + kiểm tra policy), đi kèm `POST /v1/loans`: kind `decision_package`.

**Không được làm**
- Không vượt trần ML, không tự đặt lãi suất.
- Không tự phê duyệt khoản tầng 2 vượt ngưỡng.
- Không hứa trước khi duyệt xong.

**Chuyển con người khi:** vượt ngưỡng tiền, PD sát ngưỡng từ chối, kiểm tra độc lập không đồng ý với tờ trình (xem mục 6.1).

---

## 4. Agent 3 — Giải ngân, Thu hồi & Giám sát sau vay

**Tương đương:** bộ phận tác nghiệp + quản lý rủi ro sau cho vay + chăm sóc / thu hồi nợ.

**Việc chính**
- **Giải ngân:**
  - Khi người bán bấm chấp nhận, phát lệnh giải ngân **1 lần, thẳng tới nhà cung cấp**, đúng số tiền đã duyệt.
  - Trước đó kiểm tra nhà cung cấp: tồn tại thật, không liên kết với người bán, giá nhập hợp lý, có lịch sử giao dịch.
- **Theo dõi giao hàng:** nhà cung cấp phải gửi hàng trong số ngày cam kết (kiểm tra 5).
- **Thu hồi:**
  - Theo dõi lịch trích nợ theo tỷ lệ và sàn lũy kế (phần tính toán do code làm).
  - Giải thích từng lần trích cho người bán, nhắc lịch trả.
- **Giám sát:**
  - Kiểm tra covenants hằng ngày, phân tích nguyên nhân khi có cảnh báo.
  - Phân biệt mùa vụ, sự cố tạm thời hay suy yếu thật.
- **Cảnh báo theo 3 mức:**
  - Xanh: tự theo dõi.
  - Vàng: soạn báo cáo nguyên nhân.
  - Đỏ: báo ngay cán bộ.
- **Soạn phương án giãn nợ** khi người bán khó khăn thật, để cán bộ duyệt. Xác nhận tất toán.

**Ghi lên FISCO**
- `/disburse`, `/deliveries`, `/repayments` (kèm `notice` giải thích).
- `/floor-checkpoints`, `/alerts`, `/covenant-events`.

**Không được làm**
- Không đổi số tiền hay điều khoản đã duyệt.
- Không tự **thu hồi trước hạn, tuyên bố vỡ nợ hay giãn nợ**: chỉ đề xuất, cán bộ quyết (`/accelerate`, `/default`, `/restructure` chỉ dùng được bằng khóa cán bộ).
- Không miễn giảm lãi phí, không đe dọa, không liên hệ người thứ ba.

**Chuyển con người khi:** cảnh báo Đỏ, vi phạm covenant, giao hàng trễ hoặc không giao, người bán xin giãn nợ.

---

## 5. Agent 4 — Hậu kiểm & Cải tiến

**Tương đương:** bộ phận kiểm soát nội bộ / model risk.

**Việc chính**
- Định kỳ rà soát các khoản đã cho vay, kể cả khoản trả tốt lẫn khoản trễ hạn hay vỡ nợ: lúc duyệt agent nói gì, thực tế ra sao.
- Tìm pattern:
  - nhóm nào bị đánh giá sai
  - cờ nào hay bị bỏ sót
  - lời giải thích nào gây khiếu nại
- Theo dõi tỷ lệ nợ xấu của nhóm duyệt tự động. Vượt mức cho phép thì báo để **dừng duyệt tự động** (đây là 1 trong 6 điều kiện bắt buộc chuyển con người).
- Đối chiếu tỷ lệ sai giữa agent và cán bộ trong giai đoạn chạy song song.
- Viết **báo cáo đề xuất** cho Agent 1–3, ví dụ chỉnh prompt, thêm câu hỏi bổ sung, thêm cờ kiểm tra.
- Kiểm thử đối kháng định kỳ bằng tài liệu chèn lệnh ẩn.

**Đọc từ FISCO:** toàn bộ lịch sử khoản vay, trả nợ, cảnh báo và audit trail (`GET /v1/loans`, `/v1/portfolio`, `/v1/audit/...`).

**Không được làm**
- Không tự sửa prompt, ngưỡng hay mô hình của agent khác khi đang chạy.
  - Mọi đề xuất phải được con người duyệt, test, đánh version rồi mới áp dụng.
  - Thay đổi mô hình ML phải qua hội đồng thẩm định mô hình của WeBank.
- Không huấn luyện trên dữ liệu định danh cá nhân (theo hướng dẫn an toàn AI 18/6/2026).

---

## 6. Phần KHÔNG phải agent

| Thành phần | Làm gì |
|---|---|
| Orchestrator (code cố định) | Điều phối bước, gọi ML/agent, áp 6 điều kiện chuyển con người, ghi FISCO |
| ML core | PD, dự báo mùa vụ, gian lận đồ thị, định giá lãi, cảnh báo sớm, hạn mức trần |
| Policy engine (code) | Luật cứng từ chối, ngưỡng tiền, trần lãi, phân nhóm 1–4 |
| Bộ trích xuất tài liệu | Tài liệu thành trường cố định; không có tool, không ra quyết định |
| Cán bộ WeBank | Ký khoản lớn, xử lý cảnh báo Đỏ, quyết định thu hồi trước hạn / vỡ nợ / giãn nợ, xem xét lại theo yêu cầu người bán |
| FISCO BCOS | Sổ cái chung, không sửa được: khoản vay, giải ngân, trả nợ, cảnh báo, hash tài liệu |

### 6.1. Lưu ý: kiểm tra độc lập (phản biện)

Bản 6 agent có **Agent 3 Phản biện**: đánh giá độc lập *trước khi* đọc tờ trình. Luật vay vốn lưu động 2024 yêu cầu tách thẩm định khỏi phê duyệt. Bản 4 agent đã bỏ vai này, nên cần chọn 1 trong 3 cách:

- **(a)** Thêm 1 lượt "phản biện" chạy độc lập bên trong Agent 2: model hoặc prompt khác, không thấy tờ trình. Agent 2 ≠ phản biện thì chuyển con người.
- **(b)** Coi cán bộ ký là bước phản biện, nhưng như vậy mọi khoản tầng 2 đều phải có người ký.
- **(c)** Quay lại 5 agent, tách Phản biện thành agent riêng.

Đề xuất: **(a)** cho PoC, vì giữ được 4 agent mà vẫn có kiểm tra chéo.

---

## 7. Thay đổi so với bản nháp và lý do

| Bản nháp | Đã sửa thành | Lý do |
|---|---|---|
| A1 lấy điểm tín dụng "từ AnGroup hay các group khác" | **Giữ Ant Group**, nhưng đi qua **钱塘征信** (tổ chức tín dụng có vốn Ant, có giấy phép); các nguồn khác qua NHND / 百行 / 朴道 | Quy định 断直连: giấy phép nằm ở **bên cung cấp dữ liệu**. Nền tảng (kể cả Ant) không được đưa thẳng dữ liệu cá nhân cho ngân hàng, phải qua tổ chức tín dụng có phép. Ngân hàng chỉ cần ủy quyền của người bán |
| A1 "gọi API FATE để xem có nợ xấu" | Nợ xấu lấy từ **báo cáo tín dụng**; FATE chỉ trả **điểm rủi ro kết hợp + tín hiệu bất thường** | FATE là học liên kết, không phải nơi tra cứu nợ xấu; WeBank chỉ nhận kết quả mô hình, không nhận dữ liệu |
| A1 "điểm thấp quá threshold thì deny" | Policy engine (code) quyết từ chối theo luật cứng; A1 chỉ báo cáo | Agent không là bên quyết định; ngưỡng phải nhất quán và kiểm toán được |
| A1 "gửi thông tin lên FISCO" | **Mọi bước** đều ghi FISCO, qua orchestrator | Audit trail phải liên tục từ nộp hồ sơ tới tất toán; agent không giữ khóa chain |
| A2 "người vay được duyệt bao nhiêu" | A2 **đề xuất ≤ trần ML**; cán bộ ký khi vượt ngưỡng | Agent không tự tính con số, không tự phê duyệt |
| A2 chatbot giải thích | Giữ nguyên + làm rõ: giải thích ở mức yếu tố, không lộ ngưỡng, có quyền xem xét lại | Điều 24 Luật BVTTCN |
| A3 "giải ngân từ các ngân hàng" | **WeBank là bên cho vay duy nhất**, giải ngân 1 lần **thẳng nhà cung cấp** | Quyết định 28.09 (bỏ mô hình nhiều ngân hàng); tiền dùng đúng mục đích |
| A3 "quản lý trả nợ, báo vấn đề" | Thêm giám sát covenants, cảnh báo 3 mức, theo dõi giao hàng, soạn giãn nợ | Gộp vai Agent 4–6 của bản 6 agent; thu hồi trước hạn / vỡ nợ vẫn cần người |
| A4 "rút kinh nghiệm, khuyên A1–A3" | Giữ nguyên + đề xuất phải qua người duyệt; thêm theo dõi nợ xấu nhóm tự động, kiểm thử đối kháng | Agent không tự sửa agent khác khi đang chạy (kiểm soát thay đổi, hướng dẫn AI 2026) |
| (không có) | Thêm kiểm tra độc lập (mục 6.1) | Bản 4 agent mất vai phản biện mà luật yêu cầu |

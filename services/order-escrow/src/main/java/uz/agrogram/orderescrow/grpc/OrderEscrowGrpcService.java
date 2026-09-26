package uz.agrogram.orderescrow.grpc;

import io.grpc.stub.StreamObserver;
import java.util.function.Supplier;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.service.EscrowService;
import uz.agrogram.orderescrow.v1.ConfirmHandoverRequest;
import uz.agrogram.orderescrow.v1.ConfirmHandoverResponse;
import uz.agrogram.orderescrow.v1.ConfirmReceiptRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptResponse;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.CreateTransactionResponse;
import uz.agrogram.orderescrow.v1.GetTransactionRequest;
import uz.agrogram.orderescrow.v1.GetTransactionResponse;
import uz.agrogram.orderescrow.v1.ListTransactionsRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsResponse;
import uz.agrogram.orderescrow.v1.OrderEscrowServiceGrpc;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.PayInResponse;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptRequest;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptResponse;

/**
 * agrogram.orderescrow.v1.OrderEscrowService over gRPC: maps each RPC to
 * {@link EscrowService} with the caller's {@link Principal}. Every failure
 * leaves as a status with exactly one agrogram.common.v1.Error in its details;
 * anything unexpected (including a bug that throws) is INTERNAL, like the Go
 * services' recovery interceptor.
 */
public final class OrderEscrowGrpcService extends OrderEscrowServiceGrpc.OrderEscrowServiceImplBase {

    private final EscrowService service;

    public OrderEscrowGrpcService(EscrowService service) {
        this.service = service;
    }

    @Override
    public void createTransaction(CreateTransactionRequest request,
            StreamObserver<CreateTransactionResponse> responseObserver) {
        unary(responseObserver, () -> service.createTransaction(Principal.current(), request));
    }

    @Override
    public void payIn(PayInRequest request, StreamObserver<PayInResponse> responseObserver) {
        unary(responseObserver, () -> service.payIn(Principal.current(), request));
    }

    @Override
    public void submitWeighbridgeReceipt(SubmitWeighbridgeReceiptRequest request,
            StreamObserver<SubmitWeighbridgeReceiptResponse> responseObserver) {
        unary(responseObserver, () -> service.submitWeighbridgeReceipt(Principal.current(), request));
    }

    @Override
    public void confirmHandover(ConfirmHandoverRequest request,
            StreamObserver<ConfirmHandoverResponse> responseObserver) {
        unary(responseObserver, () -> service.confirmHandover(Principal.current(), request));
    }

    @Override
    public void confirmReceipt(ConfirmReceiptRequest request, StreamObserver<ConfirmReceiptResponse> responseObserver) {
        unary(responseObserver, () -> service.confirmReceipt(Principal.current(), request));
    }

    @Override
    public void getTransaction(GetTransactionRequest request, StreamObserver<GetTransactionResponse> responseObserver) {
        unary(responseObserver, () -> service.getTransaction(Principal.current(), request));
    }

    @Override
    public void listTransactions(ListTransactionsRequest request,
            StreamObserver<ListTransactionsResponse> responseObserver) {
        unary(responseObserver, () -> service.listTransactions(Principal.current(), request));
    }

    private static <T> void unary(StreamObserver<T> observer, Supplier<T> call) {
        T response;
        try {
            response = call.get();
        } catch (VirtualMachineError e) {
            throw e;
        } catch (Throwable t) {
            AppException e = AppException.from(t);
            CallInfo.recordError(e);
            observer.onError(e.toStatusException());
            return;
        }
        observer.onNext(response);
        observer.onCompleted();
    }
}
